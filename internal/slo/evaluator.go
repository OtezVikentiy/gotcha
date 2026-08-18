package slo

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
		return e.close(ctx, p, s, now, d)
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
	budget, attainment, remaining := e.fullWindowBudget(ctx, p, s, now)
	inc, created, err := e.Store.OpenIncident(ctx, s.ID, s.ProjectID, d.BurnShort, budget)
	if err != nil {
		slog.Error("slo evaluator: open incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !created {
		return false // гонка: параллельный тик успел открыть — не дублируем уведомление
	}
	e.notify(ctx, s, inc, true, attainment, remaining, d.BurnShort)
	return true
}

// close закрывает открытый инцидент. Бюджет за полное окно считается только по
// факту закрытия (перехода) — не каждый тик.
func (e *Evaluator) close(ctx context.Context, p Provider, s SLO, now time.Time, d BurnDecision) bool {
	inc, resolved, err := e.Store.ResolveIncident(ctx, s.ID)
	if err != nil {
		slog.Error("slo evaluator: resolve incident failed", "slo_id", s.ID, "error", err)
		return false
	}
	if !resolved {
		return false // открытого не было — закрывать нечего
	}
	_, attainment, remaining := e.fullWindowBudget(ctx, p, s, now)
	e.notify(ctx, s, inc, false, attainment, remaining, d.BurnShort)
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

// notify рассылает событие (если нотифаер задан) и помечает инцидент, чтобы алерт
// ушёл ровно один раз на открытие и закрытие. nil-гвард: T5 подставит реальный
// нотифаер, а до того (и в тестах) оценщик обязан работать со stub/nil.
func (e *Evaluator) notify(ctx context.Context, s SLO, inc Incident, opened bool, attainment, remaining, burn float64) {
	if e.Notifier != nil {
		e.Notifier.Notify(ctx, SLOEvent{
			SLO:             s,
			Incident:        inc,
			Opened:          opened,
			Attainment:      attainment,
			BudgetRemaining: remaining,
			BurnRate:        burn,
		})
	}
	if err := e.Store.MarkNotified(ctx, inc.ID, opened); err != nil {
		slog.Error("slo evaluator: mark notified failed", "incident_id", inc.ID, "error", err)
	}
}
