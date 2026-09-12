package slo

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// SLO живут на скользящих окнах в дни, дорогой минутный такт не нужен: двух
// минут достаточно, чтобы burn-rate инцидент открылся почти вовремя.
const defaultSLOInterval = 2 * time.Minute

// сколько тиков подряд короткое окно должно оставаться ниже порога, прежде чем
// инцидент реально закроется — гистерезис против флапа на грани порога.
const defaultCloseStreak = 3

// без пола тик может зависнуть навсегда: Provider.Buckets бьёт по CH без своего таймаута.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// час крупнее burn-шага: полный бюджет считается только на переходе, не на
// каждом тике, точности до часа достаточно и это на порядок дешевле.
const fullWindowStep = time.Hour

type SLOEvent struct {
	SLO             SLO
	Incident        Incident
	Opened          bool    // true — инцидент открыт; false — закрыт
	Attainment      float64 // достижение за полное окно на момент перехода
	BudgetRemaining float64 // доля оставшегося бюджета за полное окно (1=цел, 0=исчерпан, <0=перерасход)
	BurnRate        float64 // burn rate короткого (fast) окна на момент перехода
}

// nil-совместим: оценщик работает и без нотифаера (тесты, инсталляции без каналов).
type Notifier interface {
	Notify(ctx context.Context, ev SLOEvent)

	// Evaluator шлёт ступень лесенки и адресованный recovery через эти методы,
	// не зовёт Notify напрямую на открытии/закрытии (см. notifyOpen/notifyClose).
	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error)
	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

// duck-typed локально, как MaintenanceChecker: slo не импортирует incidentgroup.
// nil-совместим — деградированная сборка без групп работает как раньше.
type sloGroupHook interface {
	Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error)
}

type Evaluator struct {
	Pool      *pgxpool.Pool
	Store     *Store
	Providers map[SLIKind]Provider
	Notifier  Notifier
	Interval  time.Duration
	Maint     MaintenanceChecker

	// nil-совместим: без него открытие просто не уведомляет об эскалации.
	Policy *escalation.PolicyStore

	// членство свежеоткрытого uptime-инцидента в группе down-корня его монитора;
	// уведомление уходит только при «не maintenance И не grouped» — порядок проверок не важен.
	IncidentGroups sloGroupHook

	// ленивая инициализация в Tick: структуру собирают литералом без этого поля.
	closeStreak map[int64]int

	lastTickUnix    atomic.Int64
	lastTickSeconds atomic.Uint64 // math.Float64bits: atomic.Uint64 не хранит float64
}

func (e *Evaluator) Run(ctx context.Context) {
	interval := e.Interval
	if interval <= 0 {
		interval = defaultSLOInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := e.Tick(ctx); err != nil {
				slog.Error("slo evaluator: tick failed", "error", err)
			}
		}
	}
}

// умерший или отставший оценщик снаружи выглядит ровно как «по всем SLO
// спокойно» — тишина и есть его нормальный вывод.
func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

// приближение к Interval означает, что оценщик перестаёт укладываться в период.
func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

func (e *Evaluator) tickBudget() time.Duration {
	interval := e.Interval
	if interval <= 0 {
		interval = defaultSLOInterval
	}
	budget := time.Duration(float64(interval) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

func (e *Evaluator) Tick(ctx context.Context) (int, error) {
	if e.closeStreak == nil {
		e.closeStreak = make(map[int64]int)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()
	slos, err := e.Store.ListEnabled(ctx)
	if err != nil {
		e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		return 0, err
	}
	now := time.Now().UTC()
	transitions := 0
	for _, s := range slos {
		if e.evalSLO(ctx, s, now) {
			transitions++
		}
	}
	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		// отметку живости не публикуем — иначе обрывающийся тик выглядел бы здоровым.
		slog.Warn("slo evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "slos", len(slos))
		return transitions, nil
	}
	e.lastTickUnix.Store(time.Now().Unix())
	return transitions, nil
}

func (e *Evaluator) evalSLO(ctx context.Context, s SLO, now time.Time) bool {
	p, ok := e.Providers[s.Kind]
	if !ok {
		return false
	}
	long, short, err := e.burnWindows(ctx, p, s, now)
	if err != nil {
		slog.Error("slo evaluator: burn buckets failed", "slo_id", s.ID, "error", err)
		return false
	}
	d := DecideBurn(long, short, s.Target, s.BurnThreshold)

	switch {
	case d.OpenSignal:
		e.closeStreak[s.ID] = 0
		return e.open(ctx, p, s, now, d)
	case d.CloseSignal:
		e.closeStreak[s.ID]++
		if e.closeStreak[s.ID] < defaultCloseStreak {
			return false // рано: короткое окно должно остыть N тиков подряд
		}
		e.closeStreak[s.ID] = 0
		return e.close(ctx, s)
	default:
		// Короткое окно ещё горит, но длинное не подтвердило (или наоборот) —
		// инцидент, если открыт, держим; счётчик остывания сбрасываем.
		e.closeStreak[s.ID] = 0
		return false
	}
}

// long — весь ряд корзин (slow-окно), short — корзины из последних shortMin
// минут. Не «последняя выжившая корзина»: дырка в потоке (умер Runner, лёг CH)
// не должна тихо растягивать fast-окно в прошлое — тогда short остаётся пуст.
func (e *Evaluator) burnWindows(ctx context.Context, p Provider, s SLO, now time.Time) (long, short []Bucket, err error) {
	longMin, shortMin := s.BurnLongMin, s.BurnShortMin
	if longMin <= 0 {
		longMin = 60
	}
	if shortMin <= 0 {
		shortMin = 5
	}
	from := now.Add(-time.Duration(longMin) * time.Minute)
	step := time.Duration(shortMin) * time.Minute
	bs, err := p.Buckets(ctx, s, from, now, step)
	if err != nil {
		return nil, nil, err
	}
	long = bs
	short = recentBuckets(bs, now, step)
	return long, short, nil
}

// bs идёт по возрастанию T; T — начало интервала корзины, не момент последних
// данных в ней, поэтому в short входит корзина, чей КОНЕЦ (T+step) позже начала
// окна свежести (now-step) — эквивалентно строгому T > now-2*step. Строго: конец
// корзины ровно на границе окна не считается свежим, иначе K66 (растянутое
// «короткое» окно) вернётся под видом запаса на выравнивание сетки.
func recentBuckets(bs []Bucket, now time.Time, step time.Duration) []Bucket {
	cutoff := now.Add(-2 * step)
	i := len(bs)
	for i > 0 && bs[i-1].T.After(cutoff) {
		i--
	}
	return bs[i:]
}

// проверка OpenIncidentFor впереди гарантирует, что дорогой запрос за полным
// окном не летит на каждом тике, пока инцидент уже открыт.
func (e *Evaluator) open(ctx context.Context, p Provider, s SLO, now time.Time, d BurnDecision) bool {
	if _, already, err := e.Store.OpenIncidentFor(ctx, s.ID); err != nil {
		slog.Error("slo evaluator: open-for failed", "slo_id", s.ID, "error", err)
		return false
	} else if already {
		return false
	}
	// attainment/remaining игнорируются — StepNotifier перечитывает инцидент и сам
	// восстанавливает их; budget персистится в slo_incidents.budget_remaining.
	budget, _, _ := e.fullWindowBudget(ctx, p, s, now)
	inMaint := e.inMaintenance(ctx, s.ProjectID, now)
	inc, created, err := e.Store.OpenIncident(ctx, s.ID, s.ProjectID, d.BurnShort, budget, inMaint)
	if err != nil {
		slog.Error("slo evaluator: open incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !created {
		return false
	}
	// состав группы собирается всегда, даже в maintenance — гейтится только уведомление.
	grouped := e.groupGate(ctx, s, inc)
	if !inMaint && !grouped {
		e.notifyOpen(ctx, s.ProjectID, inc)
	}
	return true
}

// только uptime-SLI с привязанным монитором; true — член информирующей группы
// (за неё говорит корень). Ошибка — шумим как без групп: лишний алерт лучше пропущенного.
func (e *Evaluator) groupGate(ctx context.Context, s SLO, inc Incident) bool {
	if e.IncidentGroups == nil || s.Kind != SLIUptime || s.MonitorID == nil {
		return false
	}
	attached, informing, err := e.IncidentGroups.Attach(ctx, "slo", inc.ID, "monitor", *s.MonitorID)
	if err != nil {
		slog.Error("slo evaluator: group attach failed", "incident_id", inc.ID, "error", err)
		return false
	}
	return attached && informing
}

func (e *Evaluator) close(ctx context.Context, s SLO) bool {
	inc, resolved, err := e.Store.ResolveIncident(ctx, s.ID)
	if err != nil {
		slog.Error("slo evaluator: resolve incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !resolved {
		return false
	}
	e.notifyClose(ctx, inc)
	return true
}

// budget — nil, если за окном нет данных: NULL в slo_incidents честнее подставного нуля.
func (e *Evaluator) fullWindowBudget(ctx context.Context, p Provider, s SLO, now time.Time) (budget *float64, attainment, remaining float64) {
	from := now.Add(-time.Duration(s.WindowDays) * 24 * time.Hour)
	if capD := p.RetentionCap(); capD > 0 {
		if clip := now.Add(-capD); clip.After(from) {
			from = clip
		}
	}
	bs, err := p.Buckets(ctx, s, from, now, fullWindowStep)
	if err != nil {
		slog.Warn("slo evaluator: full-window budget query failed", "slo_id", s.ID, "error", err)
		return nil, 0, 0
	}
	att, _ := Attainment(bs)
	rem, ok := BudgetRemainingFraction(bs, s.Target)
	if !ok {
		return nil, att, 0
	}
	return &rem, att, rem
}

// ошибка проверки трактуется как «не в окне»: молчать о реальном прожоге
// бюджета дороже, чем лишнее уведомление.
func (e *Evaluator) inMaintenance(ctx context.Context, projectID int64, now time.Time) bool {
	if e.Maint == nil {
		return false
	}
	v, err := e.Maint.InMaintenance(ctx, projectID, now)
	if err != nil {
		slog.Error("slo evaluator: maintenance check failed, treating as not in maintenance",
			"project_id", projectID, "error", err)
		return false
	}
	return v
}

// шлёт сразу только ступень 0, если её задержка уже настала; остальные
// ступени досылает планировщик.
func (e *Evaluator) notifyOpen(ctx context.Context, projectID int64, inc Incident) {
	if e.Policy == nil || e.Notifier == nil || e.Pool == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, projectID, escalation.SeverityCritical)
	if err != nil {
		slog.Error("slo evaluator: escalation policy failed", "incident_id", inc.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "slo", e.Pool, inc.ID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return e.Notifier.NotifyStep(ctx, inc.ID, chs, step) },
		func(id int64, from int) (bool, error) { return e.Store.BumpEscalation(ctx, id, from) })
	if err != nil {
		slog.Error("slo evaluator: notify step failed", "incident_id", inc.ID, "error", err)
		return
	}
	if sent {
		if err := e.Store.MarkNotified(ctx, inc.ID, true); err != nil {
			slog.Error("slo evaluator: mark notified failed", "incident_id", inc.ID, "error", err)
		}
	}
}

// закрытие шлёт recovery только в каналы из лога эскалации; пустой набор —
// намеренное молчание.
func (e *Evaluator) notifyClose(ctx context.Context, inc Incident) {
	if e.Pool == nil || e.Notifier == nil {
		return
	}
	chs, err := escalation.RecoveryChannels(ctx, e.Pool, "slo", inc.ID)
	if err != nil {
		slog.Error("slo evaluator: recovery channels failed", "incident_id", inc.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := e.Notifier.NotifyRecovery(ctx, inc.ID, chs); err != nil {
		slog.Error("slo evaluator: notify recovery failed", "incident_id", inc.ID, "error", err)
		return
	}
	if err := e.Store.MarkNotified(ctx, inc.ID, false); err != nil {
		slog.Error("slo evaluator: mark notified failed", "incident_id", inc.ID, "error", err)
	}
}
