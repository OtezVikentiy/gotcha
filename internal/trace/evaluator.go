package trace

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// evaluatorDefaultInterval/TopK/BaselineDays — дефолты пустых полей Evaluator
// (см. поля). Interval 5 минут — компромисс между свежестью алерта и нагрузкой
// на CH: свежее окно детектора всё равно измеряется десятками минут
// (cfg.WindowMinutes), чаще тикать смысла нет.
const (
	evaluatorDefaultInterval     = 5 * time.Minute
	evaluatorDefaultTopK         = 50
	evaluatorDefaultBaselineDays = 7
)

// evaluatorVitalMetrics — web-vital'ы, которые оценщик отслеживает на регрессию.
// Сознательно только Core Web Vitals (lcp/inp/cls): fcp/ttfb собираются и
// хранятся, но НЕ оцениваются на этом этапе — у них нет такого же ясного порога
// «плохо пользователю», и они добавили бы шумных целей без ценности алерта.
var evaluatorVitalMetrics = []string{"lcp", "inp", "cls"}

// Evaluator — периодический оценщик регрессий производительности (план 4, §8).
// Каждый тик обходит топ-K нагруженных целей каждого проекта, сравнивает свежее
// окно со скользящей базой через чистую Decide и открывает/закрывает инциденты
// в perf_regressions, шля алерт ровно один раз на открытие и один на закрытие
// (защита — флаги notified_open/notified_close). Собирается в cmd/gotcha при
// Mode == uptime|all рядом с uptime.Watchdog.
//
// Оценщик работает в реальном времени (окно привязано к time.Now при тике) и НЕ
// возобновляем: пропущенные из-за простоя процесса окна не досчитываются — как
// и uptime.Watchdog, он опирается на «сейчас», а не на курсор. Для алертов о
// росте p95 этого достаточно: регрессия, продержавшаяся дольше окна, будет
// поймана следующим живым тиком.
type Evaluator struct {
	Pool        *pgxpool.Pool      // конфиг и список проектов
	Query       *Query             // агрегаты производительности из CH
	Regressions *RegressionService // инциденты в perf_regressions (PG)
	Notifier    *RegressionNotifier // nil → только инциденты, без алертов

	Interval     time.Duration // период тика, дефолт 5 минут
	TopK         int           // сколько верхних по трафику целей оценивать, дефолт 50
	BaselineDays int           // ширина окна скользящей базы, дефолт 7 дней
}

// Run тикает каждый Interval, пока не отменят ctx. Запускается как
// "go e.Run(ctx)"; отдельного Close нет — буферов, которые надо сливать, у
// оценщика нет, достаточно зависеть от ctx (как uptime.Watchdog).
func (e *Evaluator) Run(ctx context.Context) {
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
			e.tick(ctx)
		}
	}
}

// projectConfig — строка списка проектов: id и сырой perf_regression_config.
type projectConfig struct {
	id  int64
	raw []byte
}

// tick — один проход оценщика по всем проектам. Ошибка по одному
// проекту/цели/метрике логируется и не прерывает остальные (§9). Публичная
// видимость в пакете — чтобы интеграционный тест звал его напрямую вместо
// ожидания тикера.
func (e *Evaluator) tick(ctx context.Context) {
	topK := e.TopK
	if topK <= 0 {
		topK = evaluatorDefaultTopK
	}
	baselineDays := e.BaselineDays
	if baselineDays <= 0 {
		baselineDays = evaluatorDefaultBaselineDays
	}

	// Дешёвый список кандидатов: все проекты с их конфигом. Отсев по трафику —
	// уже на уровне целей (TopEndpointsByTraffic/TopVitalPages за окно), поэтому
	// distinct по CH тут не нужен; проект без данных просто не даст целей.
	projects, err := e.listProjects(ctx)
	if err != nil {
		slog.Error("trace: evaluator: list projects failed", "error", err)
		return
	}

	now := time.Now().UTC()
	for _, p := range projects {
		cfg, err := RegressionConfigFromJSON(p.raw)
		if err != nil {
			// RegressionConfigFromJSON вернул дефолты вместе с ошибкой —
			// логируем и продолжаем оценивать проект на дефолтах.
			slog.Error("trace: evaluator: parse config failed, using defaults", "project_id", p.id, "error", err)
		}
		if !cfg.Enabled {
			continue
		}
		e.evalProject(ctx, p.id, cfg, topK, baselineDays, now)
	}
}

// listProjects читает id и конфиг всех проектов. Строки вычитываются целиком до
// возврата, чтобы не держать соединение пула открытым, пока evalProject бьёт по
// нему своими запросами.
func (e *Evaluator) listProjects(ctx context.Context) ([]projectConfig, error) {
	rows, err := e.Pool.Query(ctx, `SELECT id, perf_regression_config FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectConfig
	for rows.Next() {
		var p projectConfig
		if err := rows.Scan(&p.id, &p.raw); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// evalProject оценивает топ-K эндпойнтов (p95 длительности) и топ-K
// vital-страниц (p75 lcp/inp/cls) проекта за свежее окно [now-window, now).
func (e *Evaluator) evalProject(ctx context.Context, projectID int64, cfg RegressionConfig, topK, baselineDays int, now time.Time) {
	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)

	endpoints, err := e.Query.TopEndpointsByTraffic(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top endpoints failed", "project_id", projectID, "error", err)
	}
	for _, target := range endpoints {
		recent, err := e.Query.RecentEndpointP95(ctx, projectID, target, recentFrom, now)
		if err != nil {
			slog.Error("trace: evaluator: recent endpoint p95 failed", "project_id", projectID, "target", target, "error", err)
			continue
		}
		base, err := e.Query.BaselineEndpointP95(ctx, projectID, target, baselineDays, now)
		if err != nil {
			slog.Error("trace: evaluator: baseline endpoint p95 failed", "project_id", projectID, "target", target, "error", err)
			continue
		}
		e.evalTarget(ctx, projectID, "endpoint_p95", target, metricDuration, base, recent, cfg, now)
	}

	pages, err := e.Query.TopVitalPages(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top vital pages failed", "project_id", projectID, "error", err)
	}
	for _, target := range pages {
		for _, metric := range evaluatorVitalMetrics {
			recent, err := e.Query.RecentVitalP75(ctx, projectID, target, metric, recentFrom, now)
			if err != nil {
				slog.Error("trace: evaluator: recent vital p75 failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
				continue
			}
			base, err := e.Query.BaselineVitalP75(ctx, projectID, target, metric, baselineDays, now)
			if err != nil {
				slog.Error("trace: evaluator: baseline vital p75 failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
				continue
			}
			e.evalTarget(ctx, projectID, "webvital_p75", target, metric, base, recent, cfg, now)
		}
	}
}

// evalTarget применяет решение Decide к одной цели-метрике: открывает, закрывает
// или обновляет инцидент, шля алерт ровно один раз на открытие и один на
// закрытие. Идемпотентность открытия держится на частичном уникальном индексе
// perf_regressions (created=true отдаёт ровно один процесс — он один и шлёт
// алерт), закрытия — на атомарном Resolve (closed=true отдаёт ровно один).
func (e *Evaluator) evalTarget(ctx context.Context, projectID int64, targetKind, target, metric string, base, recent RegressionSample, cfg RegressionConfig, now time.Time) {
	open, hasOpen, err := e.Regressions.OpenFor(ctx, projectID, target, metric)
	if err != nil {
		slog.Error("trace: evaluator: open-for failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
		return
	}

	switch Decide(base, recent, cfg, metric, hasOpen).Kind {
	case DecisionOpen:
		rec, created, err := e.Regressions.Open(ctx, projectID, targetKind, target, metric, base.Value, recent.Value)
		if err != nil {
			slog.Error("trace: evaluator: open regression failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
			return
		}
		if !created {
			// Инцидент уже был открыт (гонка/предыдущий тик) — только освежаем
			// метрику, алерт уже отправлял победитель.
			if err := e.Regressions.Bump(ctx, rec.ID, recent.Value); err != nil {
				slog.Error("trace: evaluator: bump on open race failed", "id", rec.ID, "error", err)
			}
			return
		}
		if e.Notifier != nil {
			ev := RegressionEvent{
				Kind:          "regression_open",
				ProjectID:     projectID,
				Target:        target,
				Metric:        metric,
				BaselineValue: base.Value,
				CurrentValue:  recent.Value,
				PctIncrease:   pctIncrease(base.Value, recent.Value),
			}
			if err := e.Notifier.Notify(ctx, ev); err != nil {
				slog.Error("trace: evaluator: open notify failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
			}
			if err := e.Regressions.MarkNotified(ctx, rec.ID, true); err != nil {
				slog.Error("trace: evaluator: mark notified open failed", "id", rec.ID, "error", err)
			}
		}

	case DecisionResolve:
		closed, err := e.Regressions.Resolve(ctx, open.ID, recent.Value)
		if err != nil {
			slog.Error("trace: evaluator: resolve regression failed", "id", open.ID, "error", err)
			return
		}
		if closed && e.Notifier != nil {
			ev := RegressionEvent{
				Kind:            "regression_close",
				ProjectID:       projectID,
				Target:          target,
				Metric:          metric,
				BaselineValue:   base.Value,
				CurrentValue:    recent.Value,
				PctIncrease:     pctIncrease(base.Value, recent.Value),
				DurationSeconds: int64(now.Sub(open.StartedAt).Seconds()),
			}
			if err := e.Notifier.Notify(ctx, ev); err != nil {
				slog.Error("trace: evaluator: close notify failed", "project_id", projectID, "target", target, "metric", metric, "error", err)
			}
			if err := e.Regressions.MarkNotified(ctx, open.ID, false); err != nil {
				slog.Error("trace: evaluator: mark notified close failed", "id", open.ID, "error", err)
			}
		}

	case DecisionNone:
		// В норме, но инцидент ещё открыт (порог пробит, но не восстановился до
		// recovery) — освежаем current/peak, чтобы UI и алерт-текст показывали
		// актуальное значение.
		if hasOpen {
			if err := e.Regressions.Bump(ctx, open.ID, recent.Value); err != nil {
				slog.Error("trace: evaluator: bump failed", "id", open.ID, "error", err)
			}
		}
	}
}

// pctIncrease — доля роста (current-base)/base, как ждёт RegressionEvent
// (форматтер домножит на 100). base здесь всегда > 0: Decide не пускает сюда
// нулевую базу.
func pctIncrease(base, current float64) float64 {
	return (current - base) / base
}
