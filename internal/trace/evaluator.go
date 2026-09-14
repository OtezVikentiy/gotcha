package trace

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// Interval 5 минут: окно детектора и так измеряется десятками минут, чаще тикать смысла нет.
const (
	evaluatorDefaultInterval     = 5 * time.Minute
	evaluatorDefaultTopK         = 50
	evaluatorDefaultBaselineDays = 7
)

// Query бьёт по ClickHouse без собственного таймаута — без пола повисший запрос
// держал бы тик бесконечно.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// Сознательно только Core Web Vitals: fcp/ttfb собираются, но не оцениваются — нет
// такого же ясного порога «плохо пользователю».
var evaluatorVitalMetrics = []string{"lcp", "inp", "cls"}

// Не *RegressionService напрямую: тест подставляет считающую обёртку (countingRegressions),
// как SpanWriter зависит от CHConn, а не от конкретного клиента ClickHouse.
type RegressionStore interface {
	OpenForProject(ctx context.Context, projectID int64) (map[RegressionKey]Regression, error)
	Open(ctx context.Context, projectID int64, targetKind, target, metric string, base, current float64, inMaintenance bool) (Regression, bool, error)
	Bump(ctx context.Context, id int64, current float64) error
	Resolve(ctx context.Context, id int64, current float64) (bool, error)
	MarkNotified(ctx context.Context, id int64, open bool) error

	// Имя отличается от Bump — тот занят обновлением current/peak_value на тике,
	// не связанным с эскалацией.
	BumpEscalation(ctx context.Context, id int64, from int) (bool, error)
}

// Работает в реальном времени (окно привязано к time.Now) и НЕ возобновляем: окна,
// пропущенные из-за простоя процесса, не досчитываются — регрессию поймает следующий тик.
type Evaluator struct {
	Pool        *pgxpool.Pool       // конфиг и список проектов
	Query       *Query              // агрегаты производительности из CH
	Regressions RegressionStore     // инциденты в perf_regressions (PG); *RegressionService в проде
	Notifier    *RegressionNotifier // nil → только инциденты, без алертов

	// Подавляет только open/close-уведомления, не сбор данных или открытие инцидента.
	Maint MaintenanceChecker

	// Nil-совместим: деградированная сборка без него просто не уведомляет об открытии.
	Policy *escalation.PolicyStore

	Interval     time.Duration // период тика, дефолт 5 минут
	TopK         int           // сколько верхних по трафику целей оценивать, дефолт 50
	BaselineDays int           // ширина окна скользящей базы, дефолт 7 дней

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// Self-метрика живости: умерший или отставший оценщик снаружи выглядит как «регрессий нет».
func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

func (e *Evaluator) tickBudget() time.Duration {
	interval := e.Interval
	if interval <= 0 {
		interval = evaluatorDefaultInterval
	}
	budget := time.Duration(float64(interval) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// Ошибка проверки трактуется как «не в окне»: молчать о реальной регрессии дороже,
// чем уведомить лишний раз.
func (e *Evaluator) inMaintenance(ctx context.Context, projectID int64, now time.Time) bool {
	if e.Maint == nil {
		return false
	}
	v, err := e.Maint.InMaintenance(ctx, projectID, now)
	if err != nil {
		slog.Error("trace: evaluator: maintenance check failed, treating as not in maintenance",
			"project_id", projectID, "error", err)
		return false
	}
	return v
}

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

type projectConfig struct {
	id  int64
	raw []byte
}

// Ошибка по одному проекту/цели/метрике логируется и не прерывает остальные.
func (e *Evaluator) tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()

	topK := e.TopK
	if topK <= 0 {
		topK = evaluatorDefaultTopK
	}
	baselineDays := e.BaselineDays
	if baselineDays <= 0 {
		baselineDays = evaluatorDefaultBaselineDays
	}

	projects, err := e.listProjects(ctx)
	if err != nil {
		slog.Error("trace: evaluator: list projects failed", "error", err)
		e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		return
	}

	now := time.Now().UTC()
	for _, p := range projects {
		cfg, err := RegressionConfigFromJSON(p.raw)
		if err != nil {
			// Дефолт Enabled=true не годится: порча jsonb вернула бы пейджинг
			// проекту, явно ВЫКЛЮЧИВШЕМУ детектор. Пропускаем тик, не включаем.
			slog.Error("trace: evaluator: parse config failed, skipping project this tick", "project_id", p.id, "error", err)
			continue
		}
		if !cfg.Enabled {
			continue
		}
		e.evalProject(ctx, p.id, cfg, topK, baselineDays, now)
	}

	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("trace: evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "projects", len(projects))
		return
	}
	e.lastTickUnix.Store(time.Now().Unix())
}

// Строки вычитываются целиком до возврата, чтобы не держать соединение пула открытым,
// пока evalProject бьёт по нему своими запросами.
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

func (e *Evaluator) evalProject(ctx context.Context, projectID int64, cfg RegressionConfig, topK, baselineDays int, now time.Time) {
	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)

	// Один запрос в PG на весь проход по проекту вместо одного на каждую цель;
	// безопасность несмотря на возможную устарелость снимка объяснена в evalTarget.
	openRegs, err := e.Regressions.OpenForProject(ctx, projectID)
	if err != nil {
		slog.Error("trace: evaluator: open regressions for project failed", "project_id", projectID, "error", err)
		return
	}

	// По два запроса на вид целей вместо двух на КАЖДУЮ цель.
	endpoints, err := e.Query.TopEndpointsByTraffic(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top endpoints failed", "project_id", projectID, "error", err)
	}
	// Цель с уже открытой регрессией держим в оценке, даже выпав из top-K —
	// иначе снятый/переименованный/вытесненный трафиком target не закрылся бы.
	evalEndpoints := endpoints
	haveEndpoint := make(map[string]bool, len(endpoints))
	for _, t := range endpoints {
		haveEndpoint[t] = true
	}
	for key := range openRegs {
		if key.Metric != metricDuration || haveEndpoint[key.Target] {
			continue
		}
		haveEndpoint[key.Target] = true
		evalEndpoints = append(evalEndpoints, key.Target)
	}
	if len(evalEndpoints) > 0 {
		recents, err := e.Query.RecentEndpointP95s(ctx, projectID, evalEndpoints, recentFrom, now)
		if err != nil {
			slog.Error("trace: evaluator: recent endpoint p95s failed", "project_id", projectID, "error", err)
			recents = nil
		}
		// Скользящий base не должен расти поверх уже открытого инцидента — иначе
		// устойчивая (многодневная) регрессия рассасывается в собственной базе.
		rollingCutoff := recentFrom
		for _, target := range evalEndpoints {
			if r, ok := openRegs[RegressionKey{Target: target, Metric: metricDuration}]; ok && r.StartedAt.Before(rollingCutoff) {
				rollingCutoff = r.StartedAt
			}
		}
		var bases map[string]RegressionSample
		if cfg.SeasonalEnabled {
			// Сезонный base: то же окно того же дня недели за прошлые недели.
			bases, err = e.Query.SeasonalBaselineEndpointP95s(ctx, projectID, evalEndpoints, cfg.WindowMinutes, cfg.SeasonalWeeks, now)
			if err == nil {
				// Цели с недобором сезонной истории добираем скользящим base — иначе новая
				// цель без прошлых недель молчала бы.
				var undershoot []string
				for _, tx := range evalEndpoints {
					if bases[tx].Samples < cfg.MinSamples {
						undershoot = append(undershoot, tx)
					}
				}
				if len(undershoot) > 0 {
					rolling, rerr := e.Query.BaselineEndpointP95s(ctx, projectID, undershoot, baselineDays, rollingCutoff)
					if rerr != nil {
						// Добор не удался — оставляем сезонные (недобранные)
						// значения: мало сэмплов → Decide вернёт None, не паника.
						slog.Error("trace: evaluator: rolling fallback endpoint p95s failed", "project_id", projectID, "error", rerr)
					} else {
						for _, tx := range undershoot {
							bases[tx] = rolling[tx]
						}
					}
				}
			}
		} else {
			bases, err = e.Query.BaselineEndpointP95s(ctx, projectID, evalEndpoints, baselineDays, rollingCutoff)
		}
		if err != nil {
			slog.Error("trace: evaluator: baseline endpoint p95s failed", "project_id", projectID, "error", err)
			bases = nil
		}
		if recents != nil && bases != nil {
			for _, target := range evalEndpoints {
				open, hasOpen := openRegs[RegressionKey{Target: target, Metric: metricDuration}]
				recent, ok := recents[target]
				if !ok {
					// Ни одной записи в окне: цель реально пропала, не просто вытеснена
					// (ту вернул бы evalEndpoints). current не пересчитываем — данных нет.
					if hasOpen {
						e.closeRegression(ctx, open, open.CurrentValue)
					}
					continue
				}
				e.evalTarget(ctx, projectID, "endpoint_p95", target, metricDuration, bases[target], recent, cfg, now, open, hasOpen)
			}
		}
	}

	pages, err := e.Query.TopVitalPages(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top vital pages failed", "project_id", projectID, "error", err)
	}
	// Та же защита от вечно висящего инцидента, что у endpoint_p95 выше.
	evalPages := pages
	havePage := make(map[string]bool, len(pages))
	for _, t := range pages {
		havePage[t] = true
	}
	isVitalMetric := make(map[string]bool, len(evaluatorVitalMetrics))
	for _, m := range evaluatorVitalMetrics {
		isVitalMetric[m] = true
	}
	for key := range openRegs {
		if !isVitalMetric[key.Metric] || havePage[key.Target] {
			continue
		}
		havePage[key.Target] = true
		evalPages = append(evalPages, key.Target)
	}
	if len(evalPages) == 0 {
		return
	}
	vitalRecents, err := e.Query.RecentVitalP75s(ctx, projectID, evalPages, evaluatorVitalMetrics, recentFrom, now)
	if err != nil {
		slog.Error("trace: evaluator: recent vital p75s failed", "project_id", projectID, "error", err)
		return
	}
	// см. rollingCutoff у endpoint_p95 выше.
	vitalRollingCutoff := recentFrom
	for _, target := range evalPages {
		for _, m := range evaluatorVitalMetrics {
			if r, ok := openRegs[RegressionKey{Target: target, Metric: m}]; ok && r.StartedAt.Before(vitalRollingCutoff) {
				vitalRollingCutoff = r.StartedAt
			}
		}
	}
	var vitalBases map[VitalKey]RegressionSample
	if cfg.SeasonalEnabled {
		vitalBases, err = e.Query.SeasonalBaselineVitalP75s(ctx, projectID, evalPages, evaluatorVitalMetrics, cfg.WindowMinutes, cfg.SeasonalWeeks, now)
		if err == nil {
			// BaselineVitalP75s декартова (страница×метрика), подмножество пар одним запросом
			// не добрать — собираем страницы целиком и переопределяем лишь недобравшие ключи.
			var undershootPages []string
			seen := make(map[string]bool)
			for _, page := range evalPages {
				for _, m := range evaluatorVitalMetrics {
					if vitalBases[VitalKey{Transaction: page, Metric: m}].Samples < cfg.MinSamples {
						if !seen[page] {
							seen[page] = true
							undershootPages = append(undershootPages, page)
						}
						break
					}
				}
			}
			if len(undershootPages) > 0 {
				rolling, rerr := e.Query.BaselineVitalP75s(ctx, projectID, undershootPages, evaluatorVitalMetrics, baselineDays, vitalRollingCutoff)
				if rerr != nil {
					slog.Error("trace: evaluator: rolling fallback vital p75s failed", "project_id", projectID, "error", rerr)
				} else {
					for _, page := range undershootPages {
						for _, m := range evaluatorVitalMetrics {
							key := VitalKey{Transaction: page, Metric: m}
							if vitalBases[key].Samples < cfg.MinSamples {
								vitalBases[key] = rolling[key]
							}
						}
					}
				}
			}
		}
	} else {
		vitalBases, err = e.Query.BaselineVitalP75s(ctx, projectID, evalPages, evaluatorVitalMetrics, baselineDays, vitalRollingCutoff)
	}
	if err != nil {
		slog.Error("trace: evaluator: baseline vital p75s failed", "project_id", projectID, "error", err)
		return
	}
	for _, target := range evalPages {
		for _, metric := range evaluatorVitalMetrics {
			key := VitalKey{Transaction: target, Metric: metric}
			open, hasOpen := openRegs[RegressionKey{Target: target, Metric: metric}]
			recent, ok := vitalRecents[key]
			if !ok {
				if hasOpen {
					e.closeRegression(ctx, open, open.CurrentValue)
				}
				continue
			}
			e.evalTarget(ctx, projectID, "webvital_p75", target, metric, vitalBases[key], recent, cfg, now, open, hasOpen)
		}
	}
}

// open/hasOpen приходят из снимка, прочитанного один раз на проект — безопасно даже при
// устаревании: Open бьётся в ON CONFLICT DO NOTHING, Resolve — в WHERE status='open'.
func (e *Evaluator) evalTarget(ctx context.Context, projectID int64, targetKind, target, metric string, base, recent RegressionSample, cfg RegressionConfig, now time.Time, open Regression, hasOpen bool) {
	switch Decide(base, recent, cfg, metric, hasOpen).Kind {
	case DecisionOpen:
		inMaint := e.inMaintenance(ctx, projectID, now)
		rec, created, err := e.Regressions.Open(ctx, projectID, targetKind, target, metric, base.Value, recent.Value, inMaint)
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
		if !inMaint {
			e.notifyOpen(ctx, projectID, rec)
		}

	case DecisionResolve:
		e.closeRegression(ctx, open, recent.Value)

	case DecisionNone:
		// Порог пробит, но не восстановился до recovery — освежаем current/peak.
		if hasOpen {
			if err := e.Regressions.Bump(ctx, open.ID, recent.Value); err != nil {
				slog.Error("trace: evaluator: bump failed", "id", open.ID, "error", err)
			}
		}
	}
}

// Шлёт РОВНО ступень 0, если её задержка уже настала; остальные ступени досылает планировщик.
func (e *Evaluator) notifyOpen(ctx context.Context, projectID int64, rec Regression) {
	if e.Policy == nil || e.Notifier == nil || e.Pool == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, projectID, escalation.SeverityWarning)
	if err != nil {
		slog.Error("trace: evaluator: escalation policy failed", "id", rec.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "trace", e.Pool, rec.ID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return e.Notifier.NotifyStep(ctx, rec.ID, chs, step) },
		func(id int64, from int) (bool, error) { return e.Regressions.BumpEscalation(ctx, id, from) })
	if err != nil {
		slog.Error("trace: evaluator: notify step failed", "id", rec.ID, "error", err)
		return
	}
	if sent {
		if err := e.Regressions.MarkNotified(ctx, rec.ID, true); err != nil {
			slog.Error("trace: evaluator: mark notified open failed", "id", rec.ID, "error", err)
		}
	}
}

func (e *Evaluator) closeRegression(ctx context.Context, open Regression, current float64) {
	closed, err := e.Regressions.Resolve(ctx, open.ID, current)
	if err != nil {
		slog.Error("trace: evaluator: resolve regression failed", "id", open.ID, "error", err)
		return
	}
	if closed {
		e.notifyClose(ctx, open)
	}
}

// Пустой набор каналов — молчание: если открытие никому не ушло, отправлять «закрыт» нечего.
func (e *Evaluator) notifyClose(ctx context.Context, open Regression) {
	if e.Pool == nil || e.Notifier == nil {
		return
	}
	chs, err := escalation.RecoveryChannels(ctx, e.Pool, "trace", open.ID)
	if err != nil {
		slog.Error("trace: evaluator: recovery channels failed", "id", open.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := e.Notifier.NotifyRecovery(ctx, open.ID, chs); err != nil {
		slog.Error("trace: evaluator: notify recovery failed", "id", open.ID, "error", err)
		return
	}
	if err := e.Regressions.MarkNotified(ctx, open.ID, false); err != nil {
		slog.Error("trace: evaluator: mark notified close failed", "id", open.ID, "error", err)
	}
}

// base здесь всегда > 0: Decide не пускает сюда нулевую базу.
func pctIncrease(base, current float64) float64 {
	return (current - base) / base
}
