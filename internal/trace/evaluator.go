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

// RegressionStore — минимум интерфейса RegressionService, нужный Evaluator.
// Не *RegressionService напрямую (как раньше): находка №43 потребовала теста,
// считающего запросы к PostgreSQL за тик — то же, зачем SpanWriter зависит от
// CHConn, а не от конкретного клиента ClickHouse (см. writer.go). Тест
// подставляет считающую обёртку (countingRegressions, evaluator_test.go) —
// поднимать ради подсчёта настоящую PostgreSQL с трассировкой запросов было
// бы тяжелее и хрупче.
type RegressionStore interface {
	OpenForProject(ctx context.Context, projectID int64) (map[RegressionKey]Regression, error)
	Open(ctx context.Context, projectID int64, targetKind, target, metric string, base, current float64, inMaintenance bool) (Regression, bool, error)
	Bump(ctx context.Context, id int64, current float64) error
	Resolve(ctx context.Context, id int64, current float64) (bool, error)
	MarkNotified(ctx context.Context, id int64, open bool) error
}

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
	Pool        *pgxpool.Pool       // конфиг и список проектов
	Query       *Query              // агрегаты производительности из CH
	Regressions RegressionStore     // инциденты в perf_regressions (PG); *RegressionService в проде
	Notifier    *RegressionNotifier // nil → только инциденты, без алертов

	// Maint — окна обслуживания проекта (B3): подавляет open/close-уведомления,
	// не подавляет сбор данных/открытие инцидента. nil (дефолт) — окна не
	// подавляют ничего, обратная совместимость со сборками без maintenance.
	Maint MaintenanceChecker

	Interval     time.Duration // период тика, дефолт 5 минут
	TopK         int           // сколько верхних по трафику целей оценивать, дефолт 50
	BaselineDays int           // ширина окна скользящей базы, дефолт 7 дней
}

// inMaintenance — проект сейчас в окне обслуживания (B3), для гейта open/close-
// notify в evalTarget. Ошибка проверки НЕ отменяет открытие/закрытие инцидента:
// она лишь означает, что не удалось выяснить, плановые ли это работы, и
// трактуется как «не в окне» — молчать о реальной регрессии дороже, чем
// уведомить лишний раз (то же решение, что host.Evaluator.inMaintenance).
// Maint==nil (деградированная сборка) — тот же результат.
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

	// Снимок открытых регрессий проекта — один запрос в PG на весь проход по
	// проекту вместо одного на каждую цель (находка №43, см. докблок
	// RegressionStore.OpenForProject). Идемпотентность снимка в пределах
	// этого тика объяснена в докблоке evalTarget — коротко: цели этого прохода
	// не пересекаются, поэтому ничто из обработанного НИЖЕ по этому же снимку
	// не может сделать его устаревшим ДО того, как до этой же цели дойдёт
	// очередь (а до неё дойдёт ровно один раз).
	openRegs, err := e.Regressions.OpenForProject(ctx, projectID)
	if err != nil {
		slog.Error("trace: evaluator: open regressions for project failed", "project_id", projectID, "error", err)
		return
	}

	// По два запроса на вид целей вместо двух на КАЖДУЮ цель. При топ-20
	// эндпойнтов и трёх web-vital'ах прежний тик стоил больше двух сотен
	// последовательных обращений к ClickHouse на один проект — и так по всем
	// проектам подряд.
	endpoints, err := e.Query.TopEndpointsByTraffic(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top endpoints failed", "project_id", projectID, "error", err)
	}
	if len(endpoints) > 0 {
		recents, err := e.Query.RecentEndpointP95s(ctx, projectID, endpoints, recentFrom, now)
		if err != nil {
			slog.Error("trace: evaluator: recent endpoint p95s failed", "project_id", projectID, "error", err)
			recents = nil
		}
		var bases map[string]RegressionSample
		if cfg.SeasonalEnabled {
			// Сезонный base: то же окно того же дня недели за прошлые недели.
			bases, err = e.Query.SeasonalBaselineEndpointP95s(ctx, projectID, endpoints, cfg.WindowMinutes, cfg.SeasonalWeeks, now)
			if err == nil {
				// Fallback: цели с недобором сезонной истории (< min_samples в
				// слоте) добираем скользящим base одним запросом и подменяем
				// только их — иначе новая цель без прошлых недель молчала бы.
				var undershoot []string
				for _, tx := range endpoints {
					if bases[tx].Samples < cfg.MinSamples {
						undershoot = append(undershoot, tx)
					}
				}
				if len(undershoot) > 0 {
					rolling, rerr := e.Query.BaselineEndpointP95s(ctx, projectID, undershoot, baselineDays, now)
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
			bases, err = e.Query.BaselineEndpointP95s(ctx, projectID, endpoints, baselineDays, now)
		}
		if err != nil {
			slog.Error("trace: evaluator: baseline endpoint p95s failed", "project_id", projectID, "error", err)
			bases = nil
		}
		if recents != nil && bases != nil {
			for _, target := range endpoints {
				// Цель без свежих данных пропускается: так же вело себя и
				// поштучное чтение, только там пустой результат приезжал
				// отдельным запросом.
				recent, ok := recents[target]
				if !ok {
					continue
				}
				open, hasOpen := openRegs[RegressionKey{Target: target, Metric: metricDuration}]
				e.evalTarget(ctx, projectID, "endpoint_p95", target, metricDuration, bases[target], recent, cfg, now, open, hasOpen)
			}
		}
	}

	pages, err := e.Query.TopVitalPages(ctx, projectID, recentFrom, now, topK)
	if err != nil {
		slog.Error("trace: evaluator: top vital pages failed", "project_id", projectID, "error", err)
	}
	if len(pages) == 0 {
		return
	}
	vitalRecents, err := e.Query.RecentVitalP75s(ctx, projectID, pages, evaluatorVitalMetrics, recentFrom, now)
	if err != nil {
		slog.Error("trace: evaluator: recent vital p75s failed", "project_id", projectID, "error", err)
		return
	}
	var vitalBases map[VitalKey]RegressionSample
	if cfg.SeasonalEnabled {
		vitalBases, err = e.Query.SeasonalBaselineVitalP75s(ctx, projectID, pages, evaluatorVitalMetrics, cfg.WindowMinutes, cfg.SeasonalWeeks, now)
		if err == nil {
			// Fallback по СТРАНИЦАМ: BaselineVitalP75s декартова (страница×метрика),
			// подмножество пар одним запросом не добрать. Собираем уникальные
			// страницы, у которых хоть один ключ (страница,метрика) недобрал слот,
			// и добираем их скользящим — переопределяя лишь недобравшие ключи.
			var undershootPages []string
			seen := make(map[string]bool)
			for _, page := range pages {
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
				rolling, rerr := e.Query.BaselineVitalP75s(ctx, projectID, undershootPages, evaluatorVitalMetrics, baselineDays, now)
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
		vitalBases, err = e.Query.BaselineVitalP75s(ctx, projectID, pages, evaluatorVitalMetrics, baselineDays, now)
	}
	if err != nil {
		slog.Error("trace: evaluator: baseline vital p75s failed", "project_id", projectID, "error", err)
		return
	}
	for _, target := range pages {
		for _, metric := range evaluatorVitalMetrics {
			key := VitalKey{Transaction: target, Metric: metric}
			recent, ok := vitalRecents[key]
			if !ok {
				continue
			}
			open, hasOpen := openRegs[RegressionKey{Target: target, Metric: metric}]
			e.evalTarget(ctx, projectID, "webvital_p75", target, metric, vitalBases[key], recent, cfg, now, open, hasOpen)
		}
	}
}

// evalTarget применяет решение Decide к одной цели-метрике: открывает, закрывает
// или обновляет инцидент, шля алерт ровно один раз на открытие и один на
// закрытие. Идемпотентность открытия держится на частичном уникальном индексе
// perf_regressions (created=true отдаёт ровно один процесс — он один и шлёт
// алерт), закрытия — на атомарном Resolve (closed=true отдаёт ровно один).
//
// open/hasOpen приходят СНАРУЖИ (evalProject читает их одним пакетным
// OpenForProject до цикла по целям, находка №43), а не запрашиваются здесь.
// Снимок берётся один раз на проект и не устаревает в пределах одного тика:
// evalProject вызывает evalTarget не больше одного раза на каждую пару
// (target, metric) за проход (endpoints и pages — списки без повторов,
// evaluatorVitalMetrics обходится один раз на страницу), а пишет каждый вызов
// только в СВОЮ строку (Bump/Resolve всегда по open.ID именно ЭТОЙ пары) —
// поэтому ни один вызов evalTarget в рамках одного evalProject не может
// сделать снимок ДРУГОЙ, ещё не обработанной пары устаревшим. Событие,
// которое сделало бы снимок устаревшим по-настоящему (конкурентная реплика
// оценщика или ручное действие в UI между чтением снимка и записью решения),
// не опаснее, чем было при поштучном чтении: Open по-прежнему бьётся в
// ON CONFLICT DO NOTHING (проигравший просто не создаёт дубль и не шлёт
// второй алерт), а Resolve — в WHERE status='open' (закрыть уже закрытое
// само по себе безопасный no-op, closed=false). Расширение окна гонки с
// «непосредственно перед решением» до «на весь проход по проекту» не меняет
// того, что в итоге пишется в базу — обе операции сами проверяют актуальность
// при записи, а не полагаются на свежесть прочитанного.
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
		if e.Notifier != nil && !inMaint {
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
		if closed && e.Notifier != nil && !open.InMaintenance {
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
