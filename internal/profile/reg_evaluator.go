package profile

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const evaluatorDefaultInterval = 5 * time.Minute

// RegressionEvaluator периодически детектит рост self-CPU доли функций над
// скользящей базой и открывает/закрывает инциденты (калька trace.Evaluator).
// Тикер живёт в режимах uptime|all.
type RegressionEvaluator struct {
	Pool        *pgxpool.Pool
	Query       *Query
	Regressions *RegressionService
	Notifier    *RegressionNotifier
	Interval    time.Duration
	Config      RegressionConfig
}

func (e *RegressionEvaluator) Run(ctx context.Context) {
	interval := e.Interval
	if interval <= 0 {
		interval = evaluatorDefaultInterval
	}
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

// Tick — один проход по всем проектам. Ошибка по проекту не роняет остальные.
func (e *RegressionEvaluator) Tick(ctx context.Context) {
	rows, err := e.Pool.Query(ctx, "SELECT id FROM projects")
	if err != nil {
		slog.Error("profile evaluator: list projects failed", "error", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			slog.Error("profile evaluator: scan project failed", "error", err)
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("profile evaluator: iterate projects failed", "error", err)
		return
	}

	now := time.Now().UTC()
	for _, pid := range ids {
		e.evalProject(ctx, pid, now)
	}
}

func (e *RegressionEvaluator) evalProject(ctx context.Context, projectID int64, now time.Time) {
	cfg := e.Config
	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)
	services, err := e.Query.ServicesWithProfiles(ctx, projectID, recentFrom, now)
	if err != nil {
		slog.Error("profile evaluator: services failed", "project_id", projectID, "error", err)
		return
	}
	for _, st := range services {
		funcs, err := e.Query.TopFunctionsBySelfShare(ctx, projectID, st.Service, st.Type, recentFrom, now, cfg.TopK)
		if err != nil {
			slog.Error("profile evaluator: top functions failed", "project_id", projectID, "service", st.Service, "error", err)
			continue
		}
		for _, fn := range funcs {
			e.evalFunction(ctx, projectID, st.Service, st.Type, fn, recentFrom, now)
		}
	}
}

func (e *RegressionEvaluator) evalFunction(ctx context.Context, projectID int64, service, profileType, function string, recentFrom, now time.Time) {
	cfg := e.Config
	recent, samples, err := e.Query.RecentFunctionShare(ctx, projectID, service, profileType, function, recentFrom, now)
	if err != nil {
		slog.Error("profile evaluator: recent share failed", "project_id", projectID, "function", function, "error", err)
		return
	}
	base, err := e.Query.BaselineFunctionShare(ctx, projectID, service, profileType, function, cfg.BaselineDays, now)
	if err != nil {
		slog.Error("profile evaluator: baseline failed", "project_id", projectID, "function", function, "error", err)
		return
	}

	open, hasOpen, err := e.Regressions.OpenFor(ctx, projectID, service, profileType, function)
	if err != nil {
		slog.Error("profile evaluator: open-for failed", "project_id", projectID, "function", function, "error", err)
		return
	}

	switch Decide(base, recent, samples, cfg, hasOpen).Kind {
	case DecisionOpen:
		rec, created, err := e.Regressions.Open(ctx, projectID, service, profileType, function, base, recent)
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
		e.notify(ctx, projectID, service, profileType, function, base, recent, true, rec.ID)
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
			e.notify(ctx, projectID, service, profileType, function, base, recent, false, open.ID)
		}
	}
}

func (e *RegressionEvaluator) notify(ctx context.Context, projectID int64, service, profileType, function string, base, recent float64, opened bool, id int64) {
	if e.Notifier == nil {
		return
	}
	ev := ProfileRegressionEvent{
		ProjectID: projectID, Service: service, ProfileType: profileType, Function: function,
		BaselineShare: base, CurrentShare: recent, PctIncrease: pctIncrease(base, recent), Opened: opened,
	}
	if err := e.Notifier.Notify(ctx, ev); err != nil {
		slog.Error("profile evaluator: notify failed", "project_id", projectID, "function", function, "error", err)
	}
	if err := e.Regressions.MarkNotified(ctx, id, opened); err != nil {
		slog.Error("profile evaluator: mark notified failed", "id", id, "error", err)
	}
}

// pctIncrease — доля роста recent над base (0 если base<=0).
func pctIncrease(base, recent float64) float64 {
	if base <= 0 {
		return 0
	}
	return (recent - base) / base
}
