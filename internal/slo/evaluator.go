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

// defaultSLOInterval — период тика оценщика по умолчанию. SLO живут на скользящих
// окнах в дни, поэтому дорогой минутный такт (как у metric/host) не нужен: две
// минуты достаточно, чтобы burn-rate инцидент открылся почти вовремя, но не гонять
// CH попусту.
const defaultSLOInterval = 2 * time.Minute

// defaultCloseStreak — сколько тиков подряд короткое окно должно оставаться ниже
// порога, прежде чем инцидент реально закрывается (гистерезис против флапа:
// одиночный «остывший» тик на грани порога не должен схлопывать инцидент, который
// тут же откроется снова).
const defaultCloseStreak = 3

// fullWindowStep — шаг корзин при расчёте остатка бюджета за ПОЛНОЕ окно SLO.
// Час крупнее burn-шага (минуты): полный бюджет считается только в момент
// перехода (открытие/закрытие), а не каждый тик, поэтому точность до часа
// достаточна и на порядок дешевле.
const fullWindowStep = time.Hour

// SLOEvent — то, что оценщик передаёт нотифаеру на переходе инцидента. Реализация
// нотифаера — в отдельном файле (notify.go, Task 5); здесь определён минимальный
// контракт, чтобы оценщик компилировался и тестировался с заглушкой/nil.
type SLOEvent struct {
	SLO             SLO
	Incident        Incident
	Opened          bool    // true — инцидент открыт; false — закрыт
	Attainment      float64 // достижение за полное окно на момент перехода
	BudgetRemaining float64 // доля оставшегося бюджета за полное окно (1=цел, 0=исчерпан, <0=перерасход)
	BurnRate        float64 // burn rate короткого (fast) окна на момент перехода
}

// Notifier рассылает уведомление о переходе SLO-инцидента. nil-совместим:
// оценщик работает и без нотифаера (тесты, инсталляции без каналов).
type Notifier interface {
	Notify(ctx context.Context, ev SLOEvent)

	// NotifyStep/NotifyRecovery (B4, T7) — реролл open/recovery на лесенку
	// эскалации: Evaluator больше не зовёт Notify напрямую на открытии/
	// закрытии (см. notifyOpen/notifyClose), а шлёт СТУПЕНЬ лесенки и
	// адресованный recovery через них. Реализованы SLOBurnNotifier (T6).
	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) error
	NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error
}

// Evaluator периодически считает burn rate каждого включённого SLO по двум окнам
// (длинное slow + короткое fast) и открывает/закрывает инцидент сжигания бюджета,
// рассылая уведомление ровно один раз на открытие и закрытие. Та же ниша, что
// metric/host/profile-оценщики: периодическая джоба поверх PostgreSQL (стор
// определений/инцидентов) и ClickHouse (ряды good/total через провайдеры).
type Evaluator struct {
	Pool      *pgxpool.Pool
	Store     *Store
	Providers map[SLIKind]Provider
	Notifier  Notifier
	Interval  time.Duration
	Maint     MaintenanceChecker

	// Policy — политика эскалации (B4, T7): резолвит лесенку (project,
	// severity) на открытии инцидента сжигания бюджета. Nil-совместим —
	// деградированная сборка без него просто не уведомляет об открытии.
	Policy *escalation.PolicyStore

	// closeStreak — счётчик подряд идущих «остывших» тиков на SLO (гистерезис
	// флапа). Ленивая инициализация в Tick: структуру собирают литералом без
	// этого поля.
	closeStreak map[int64]int

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// Run тикает каждый Interval, пока не отменят ctx.
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

// LastTickUnix — unix-время последнего завершённого тика (0, если ни одного ещё
// не было). Self-метрика живости: умерший или отставший оценщик снаружи выглядит
// ровно как «по всем SLO спокойно» — тишина и есть его нормальный вывод.
func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

// LastTickSeconds — длительность последнего завершённого тика в секундах.
// Приближение к Interval означает, что оценщик перестаёт укладываться в период.
func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

// Tick — один проход по всем включённым SLO. Возвращает число переходов инцидентов
// (открытий+закрытий) за проход — публичный сигнал для тестов и наблюдаемости.
// Ошибка по одному SLO не роняет остальные (error-isolation, как у metric.Evaluator).
func (e *Evaluator) Tick(ctx context.Context) (int, error) {
	if e.closeStreak == nil {
		e.closeStreak = make(map[int64]int)
	}
	started := time.Now()
	slos, err := e.Store.ListEnabled(ctx)
	if err != nil {
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
	e.lastTickUnix.Store(time.Now().Unix())
	return transitions, nil
}

// evalSLO оценивает один SLO и возвращает true, если произошёл переход инцидента.
func (e *Evaluator) evalSLO(ctx context.Context, s SLO, now time.Time) bool {
	p, ok := e.Providers[s.Kind]
	if !ok {
		// Неизвестный тип SLI (например, uptime без сконфигурированного провайдера)
		// — пропуск, а не паника: оценщик соседей продолжает работать.
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

// burnWindows одним запросом достаёт ряд корзин burn-окна [now-BurnLongMin, now)
// с шагом BurnShortMin. long — весь ряд (slow-окно), short — последняя корзина
// (последнее fast-под-окно длиной BurnShortMin).
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
	if len(bs) > 0 {
		short = bs[len(bs)-1:]
	}
	return long, short, nil
}

// open открывает инцидент, если открытого ещё нет. Полный бюджет считается ТОЛЬКО
// когда инцидента ещё нет (отдельный запрос за WindowDays): проверка OpenIncidentFor
// впереди гарантирует, что дорогой запрос за полным окном не летит на каждом тике,
// пока инцидент уже открыт.
func (e *Evaluator) open(ctx context.Context, p Provider, s SLO, now time.Time, d BurnDecision) bool {
	if _, already, err := e.Store.OpenIncidentFor(ctx, s.ID); err != nil {
		slog.Error("slo evaluator: open-for failed", "slo_id", s.ID, "error", err)
		return false
	} else if already {
		return false // инцидент уже открыт — прожог продолжается, ничего нового
	}
	// attainment/remaining игнорируются: реролл (B4, T7) больше не собирает
	// SLOEvent здесь напрямую — notifyOpen шлёт ступень лесенки по incidentID,
	// а StepNotifier перечитывает инцидент и сам восстанавливает эти поля
	// (см. SLOBurnNotifier.reloadEvent). budget — единственное, что реально
	// нужно ниже: он персистится в slo_incidents.budget_remaining.
	budget, _, _ := e.fullWindowBudget(ctx, p, s, now)
	inMaint := e.inMaintenance(ctx, s.ProjectID, now)
	inc, created, err := e.Store.OpenIncident(ctx, s.ID, s.ProjectID, d.BurnShort, budget, inMaint)
	if err != nil {
		slog.Error("slo evaluator: open incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !created {
		return false // гонка: параллельный тик успел открыть — не дублируем уведомление
	}
	if !inMaint {
		e.notifyOpen(ctx, s.ProjectID, inc)
	}
	return true
}

// close закрывает открытый инцидент. Бюджет за полное окно больше не
// пересчитывается на закрытии (реролл B4, T7): recovery шлётся адресно через
// notifyClose, который перечитывает инцидент по ID и не нуждается в
// attainment/remaining, посчитанных здесь заново — в отличие от старого
// SLOEvent, собираемого evalSLO напрямую.
func (e *Evaluator) close(ctx context.Context, s SLO) bool {
	inc, resolved, err := e.Store.ResolveIncident(ctx, s.ID)
	if err != nil {
		slog.Error("slo evaluator: resolve incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !resolved {
		return false // открытого не было — закрывать нечего
	}
	if !inc.InMaintenance {
		e.notifyClose(ctx, inc)
	}
	return true
}

// fullWindowBudget считает достижение и остаток бюджета за полное окно SLO
// (WindowDays), клипуя начало окна к пределу хранения провайдера (RetentionCap>0).
// budget — nil, если за окном нет данных (Total==0): NULL в slo_incidents честнее
// подставного нуля.
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

// inMaintenance — проект сейчас в окне обслуживания (B3), для гейта open/close-
// notify в open/close. Ошибка проверки НЕ отменяет открытие инцидента: она
// лишь означает, что не удалось выяснить, плановые ли это работы, и трактуется
// как «не в окне» — молчать о реальном прожоге бюджета дороже, чем уведомить
// лишний раз (то же решение, что host.Evaluator.inMaintenance). Maint==nil
// (деградированная сборка) — тот же результат.
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

// notifyOpen — реролл (B4, T7): открытие инцидента сжигания бюджета резолвит
// лесенку эскалации (project, severity — SLO не имеют per-цель override,
// всегда table-DEFAULT slo_incidents.severity, 'critical', 0077) и шлёт РОВНО
// СТУПЕНЬ 0, если её задержка (обычно 0) уже настала; остальные ступени
// досылает планировщик (T8). Ошибка политики/уведомления не должна ронять
// оценку.
func (e *Evaluator) notifyOpen(ctx context.Context, projectID int64, inc Incident) {
	if e.Policy == nil || e.Notifier == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, projectID, escalation.SeverityCritical)
	if err != nil {
		slog.Error("slo evaluator: escalation policy failed", "incident_id", inc.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, inc.ID, 0, 0,
		func(chs []int64, step int) error { return e.Notifier.NotifyStep(ctx, inc.ID, chs, step) },
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

// notifyClose — реролл (B4, T7): закрытие инцидента сжигания бюджета шлёт
// recovery адресно, в каналы из лога эскалации (escalation.RecoveryChannels);
// пустой набор — молчание (M-7 брифа Task 6, ничего не отправлялось —
// отправлять «закрыт» нечего).
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
