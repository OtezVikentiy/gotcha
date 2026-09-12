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

// без пола тик может зависнуть навсегда: Query бьёт по ClickHouse без своего таймаута.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

type profileQuery interface {
	ActiveServices(ctx context.Context, from, to time.Time) ([]ProjectService, error)
	TopFunctionShares(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]FunctionShare, error)
	BaselineFunctionShares(ctx context.Context, projectID int64, service, profileType string, functions []string, baselineDays int, now time.Time) (map[string]BaselineShare, error)
}

type RegressionEvaluator struct {
	Query       profileQuery
	Regressions *RegressionService
	Notifier    *RegressionNotifier
	Interval    time.Duration
	Config      RegressionConfig
	Maint       MaintenanceChecker

	// nil-совместим: без него открытие просто не уведомляет об эскалации.
	Policy *escalation.PolicyStore

	// nil-совместим: без него закрытие не шлёт recovery по логу эскалации.
	Pool *pgxpool.Pool

	lastTickUnix    atomic.Int64
	lastTickSeconds atomic.Uint64 // math.Float64bits: atomic.Uint64 не хранит float64
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

func (e *RegressionEvaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

func (e *RegressionEvaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

func (e *RegressionEvaluator) tickBudget() time.Duration {
	budget := time.Duration(float64(e.interval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

func (e *RegressionEvaluator) Tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()

	now := time.Now().UTC()
	recentFrom := now.Add(-time.Duration(e.Config.WindowMinutes) * time.Minute)

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

	opens, err := e.Regressions.OpenForFunctions(ctx, ps.ProjectID, ps.Service, ps.Type, names)
	if err != nil {
		slog.Error("profile evaluator: open-for failed",
			"project_id", ps.ProjectID, "service", ps.Service, "error", err)
		return
	}

	for _, sh := range shares {
		base := baselines[sh.Function]
		open, hasOpen := opens[sh.Function]
		e.evalFunction(ctx, ps, sh, base.Share, base.Samples, open, hasOpen, now)
	}
}

func (e *RegressionEvaluator) evalFunction(ctx context.Context, ps ProjectService, sh FunctionShare, base float64, baseSamples uint64, open Regression, hasOpen bool, now time.Time) {
	cfg := e.Config
	projectID, service, profileType, function := ps.ProjectID, ps.Service, ps.Type, sh.Function
	recent, samples := sh.Share, sh.Samples

	switch Decide(base, recent, baseSamples, samples, cfg, hasOpen).Kind {
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

// ошибка проверки трактуется как «не в окне»: молчать о регрессии дороже,
// чем лишнее уведомление.
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

// шлёт сразу только ступень 0, если её задержка уже настала; остальные
// ступени досылает планировщик.
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

// закрытие шлёт recovery только в каналы из лога эскалации; пустой набор —
// намеренное молчание.
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

func pctIncrease(base, recent float64) float64 {
	if base <= 0 {
		return 0
	}
	return (recent - base) / base
}
