package profile

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

const evaluatorDefaultInterval = 5 * time.Minute

// tickBudgetShare/minTickBudget — та же пара, что host.Evaluator: дедлайн
// тика — доля Interval, но не меньше пола, иначе повисший ClickHouse-запрос
// (Query здесь без собственного таймаута) держал бы тик бесконечно.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// RegressionEvaluator периодически детектит рост self-CPU доли функций над
// скользящей базой и открывает/закрывает инциденты (калька trace.Evaluator).
// Тикер живёт в режимах uptime|all.
// profileQuery — то, что оценщику нужно от ClickHouse. Интерфейс, а не *Query,
// по той же причине, что ruleLister в пакете metric: без него у цикла Run нет
// наблюдаемого следа, и тест «поспал и убедился, что горутина вышла» оставался
// бы зелёным с вырезанным телом тика.
//
// Списка проектов из PostgreSQL здесь больше нет намеренно: обходить надо то,
// по чему есть данные, а не всё, что заведено в инсталляции.
type profileQuery interface {
	ActiveServices(ctx context.Context, from, to time.Time) ([]ProjectService, error)
	TopFunctionShares(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]FunctionShare, error)
	BaselineFunctionShares(ctx context.Context, projectID int64, service, profileType string, functions []string, baselineDays int, now time.Time) (map[string]float64, error)
}

type RegressionEvaluator struct {
	Query       profileQuery
	Regressions *RegressionService
	Notifier    *RegressionNotifier
	Interval    time.Duration
	Config      RegressionConfig
	Maint       MaintenanceChecker

	// Policy — политика эскалации (B4, T7): резолвит лесенку (project,
	// severity) на открытии регрессии. Nil-совместим — деградированная сборка
	// без него просто не уведомляет об открытии.
	Policy *escalation.PolicyStore

	// Pool — та же PG, что под Regressions: читает лог эскалации
	// incident_escalations для адресного recovery при закрытии (B4, T7, см.
	// notifyClose, escalation.RecoveryChannels). Nil-совместим.
	Pool *pgxpool.Pool

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

func (e *RegressionEvaluator) Run(ctx context.Context) {
	interval := e.interval()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.Tick(ctx)
		}
	}
}

func (e *RegressionEvaluator) interval() time.Duration {
	if e.Interval <= 0 {
		return evaluatorDefaultInterval
	}
	return e.Interval
}

// LastTickUnix — unix-время последнего завершённого тика (0, если ни одного
// ещё не было). Self-метрика живости, как у host.Evaluator/slo.Evaluator.
func (e *RegressionEvaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

// LastTickSeconds — длительность последнего завершённого тика в секундах.
func (e *RegressionEvaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

// tickBudget — дедлайн одного тика (см. tickBudgetShare/minTickBudget).
func (e *RegressionEvaluator) tickBudget() time.Duration {
	budget := time.Duration(float64(e.interval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// Tick — один проход по всем проектам. Ошибка по проекту не роняет остальные.
// Ограничен дедлайном (tickBudget): Query бьёт по CH голыми запросами без
// собственного таймаута, и без внешнего дедлайна повисший запрос держал бы
// тик (и self-метрику живости) бесконечно.
func (e *RegressionEvaluator) Tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()

	now := time.Now().UTC()
	recentFrom := now.Add(-time.Duration(e.Config.WindowMinutes) * time.Minute)

	// Работу определяют данные, а не список проектов. Раньше тик читал все
	// проекты из PostgreSQL и спрашивал ClickHouse про каждый: проект без
	// единого профиля стоил столько же, сколько нагруженный, и обход шёл по
	// всей инсталляции независимо от трафика.
	services, err := e.Query.ActiveServices(ctx, recentFrom, now)
	if err != nil {
		slog.Error("profile evaluator: active services failed", "error", err)
		e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		return
	}
	for _, ps := range services {
		e.evalService(ctx, ps, recentFrom, now)
	}

	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("profile evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "services", len(services))
		return
	}
	e.lastTickUnix.Store(time.Now().Unix())
}

// evalService проверяет один сервис одного проекта: два запроса к
// profile_samples вместо 1 + 2K.
func (e *RegressionEvaluator) evalService(ctx context.Context, ps ProjectService, recentFrom, now time.Time) {
	cfg := e.Config
	shares, err := e.Query.TopFunctionShares(ctx, ps.ProjectID, ps.Service, ps.Type, recentFrom, now, cfg.TopK)
	if err != nil {
		slog.Error("profile evaluator: top function shares failed",
			"project_id", ps.ProjectID, "service", ps.Service, "error", err)
		return
	}
	if len(shares) == 0 {
		return
	}

	names := make([]string, 0, len(shares))
	for _, sh := range shares {
		names = append(names, sh.Function)
	}
	baselines, err := e.Query.BaselineFunctionShares(ctx, ps.ProjectID, ps.Service, ps.Type, names, cfg.BaselineDays, now)
	if err != nil {
		slog.Error("profile evaluator: baseline shares failed",
			"project_id", ps.ProjectID, "service", ps.Service, "error", err)
		return
	}

	for _, sh := range shares {
		// Функции без базовой линии сравниваются с нулём — так же, как раньше
		// при пустом результате поштучного запроса.
		e.evalFunction(ctx, ps, sh, baselines[sh.Function], now)
	}
}

func (e *RegressionEvaluator) evalFunction(ctx context.Context, ps ProjectService, sh FunctionShare, base float64, now time.Time) {
	cfg := e.Config
	projectID, service, profileType, function := ps.ProjectID, ps.Service, ps.Type, sh.Function
	recent, samples := sh.Share, sh.Samples

	open, hasOpen, err := e.Regressions.OpenFor(ctx, projectID, service, profileType, function)
	if err != nil {
		slog.Error("profile evaluator: open-for failed", "project_id", projectID, "function", function, "error", err)
		return
	}

	switch Decide(base, recent, samples, cfg, hasOpen).Kind {
	case DecisionOpen:
		inMaint := e.inMaintenance(ctx, projectID, now)
		rec, created, err := e.Regressions.Open(ctx, projectID, service, profileType, function, base, recent, inMaint)
		if err != nil {
			slog.Error("profile evaluator: open failed", "project_id", projectID, "function", function, "error", err)
			return
		}
		if !created {
			if err := e.Regressions.Bump(ctx, rec.ID, recent); err != nil {
				slog.Error("profile evaluator: bump on open race failed", "id", rec.ID, "error", err)
			}
			return
		}
		if !inMaint {
			e.notifyOpen(ctx, projectID, rec)
		}
	case DecisionBump:
		if err := e.Regressions.Bump(ctx, open.ID, recent); err != nil {
			slog.Error("profile evaluator: bump failed", "id", open.ID, "error", err)
		}
	case DecisionResolve:
		closed, err := e.Regressions.Resolve(ctx, open.ID, recent)
		if err != nil {
			slog.Error("profile evaluator: resolve failed", "id", open.ID, "error", err)
			return
		}
		if closed {
			e.notifyClose(ctx, open)
		}
	}
}

// inMaintenance — проект сейчас в окне обслуживания (B3), для гейта open/close-
// notify в evalFunction. Ошибка проверки НЕ отменяет открытие регрессии: она
// лишь означает, что не удалось выяснить, плановые ли это работы, и трактуется
// как «не в окне» — молчать о реальной регрессии дороже, чем уведомить лишний
// раз (то же решение, что host.Evaluator.inMaintenance). Maint==nil
// (деградированная сборка) — тот же результат.
func (e *RegressionEvaluator) inMaintenance(ctx context.Context, projectID int64, now time.Time) bool {
	if e.Maint == nil {
		return false
	}
	v, err := e.Maint.InMaintenance(ctx, projectID, now)
	if err != nil {
		slog.Error("profile evaluator: maintenance check failed, treating as not in maintenance",
			"project_id", projectID, "error", err)
		return false
	}
	return v
}

// notifyOpen — реролл (B4, T7): открытие регрессии резолвит лесенку
// эскалации (project, severity — регрессии профиля не имеют per-функцию
// override, всегда table-DEFAULT profile_regressions.severity, 'warning',
// 0077) и шлёт РОВНО СТУПЕНЬ 0, если её задержка (обычно 0) уже настала;
// остальные ступени досылает планировщик (T8). Ошибка политики/уведомления
// не должна ронять оценку.
func (e *RegressionEvaluator) notifyOpen(ctx context.Context, projectID int64, rec Regression) {
	if e.Policy == nil || e.Notifier == nil || e.Pool == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, projectID, escalation.SeverityWarning)
	if err != nil {
		slog.Error("profile evaluator: escalation policy failed", "id", rec.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "profile", e.Pool, rec.ID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return e.Notifier.NotifyStep(ctx, rec.ID, chs, step) },
		func(id int64, from int) (bool, error) { return e.Regressions.BumpEscalation(ctx, id, from) })
	if err != nil {
		slog.Error("profile evaluator: notify step failed", "id", rec.ID, "error", err)
		return
	}
	if sent {
		if err := e.Regressions.MarkNotified(ctx, rec.ID, true); err != nil {
			slog.Error("profile evaluator: mark notified failed", "id", rec.ID, "error", err)
		}
	}
}

// notifyClose — реролл (B4, T7): закрытие регрессии шлёт recovery адресно, в
// каналы из лога эскалации (escalation.RecoveryChannels); пустой набор —
// молчание (M-7 брифа Task 6, ничего не отправлялось — отправлять «закрыт»
// нечего).
func (e *RegressionEvaluator) notifyClose(ctx context.Context, open Regression) {
	if e.Pool == nil || e.Notifier == nil {
		return
	}
	chs, err := escalation.RecoveryChannels(ctx, e.Pool, "profile", open.ID)
	if err != nil {
		slog.Error("profile evaluator: recovery channels failed", "id", open.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := e.Notifier.NotifyRecovery(ctx, open.ID, chs); err != nil {
		slog.Error("profile evaluator: notify recovery failed", "id", open.ID, "error", err)
		return
	}
	if err := e.Regressions.MarkNotified(ctx, open.ID, false); err != nil {
		slog.Error("profile evaluator: mark notified close failed", "id", open.ID, "error", err)
	}
}

// pctIncrease — доля роста recent над base (0 если base<=0).
func pctIncrease(base, recent float64) float64 {
	if base <= 0 {
		return 0
	}
	return (recent - base) / base
}
