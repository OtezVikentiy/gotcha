package profile

import (
	"context"
	"log/slog"
	"time"
)

const evaluatorDefaultInterval = 5 * time.Minute

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
	now := time.Now().UTC()
	recentFrom := now.Add(-time.Duration(e.Config.WindowMinutes) * time.Minute)

	// Работу определяют данные, а не список проектов. Раньше тик читал все
	// проекты из PostgreSQL и спрашивал ClickHouse про каждый: проект без
	// единого профиля стоил столько же, сколько нагруженный, и обход шёл по
	// всей инсталляции независимо от трафика.
	services, err := e.Query.ActiveServices(ctx, recentFrom, now)
	if err != nil {
		slog.Error("profile evaluator: active services failed", "error", err)
		return
	}
	for _, ps := range services {
		e.evalService(ctx, ps, recentFrom, now)
	}
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
		e.evalFunction(ctx, ps, sh, baselines[sh.Function])
	}
}

func (e *RegressionEvaluator) evalFunction(ctx context.Context, ps ProjectService, sh FunctionShare, base float64) {
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
