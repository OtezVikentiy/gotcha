package metric

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

const evaluatorDefaultInterval = 60 * time.Second

// Дедлайн тика — доля Interval, но не меньше пола (как у host.Evaluator): Query здесь не ставит СВОЙ
// таймаут — висящий ClickHouse-запрос иначе съедал бы тик целиком и сдвигал бы все последующие.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// Интерфейс, не конкретный тип: иначе Run не даёт наблюдаемого следа, и тест «поспал,
// проверил, что горутина вышла» остался бы зелёным даже при вырезанном тике.
type ruleLister interface {
	ListEnabled(ctx context.Context) ([]Rule, error)
}

// Членство в группах инцидентов для правил с label_key='host' — узел резолвится по hosts.name =
// label_value того же проекта. Duck-typed локально (metric не импортирует incidentgroup), nil-совместим.
type metricGroupHook interface {
	AttachMetric(ctx context.Context, incidentID, projectID int64, hostName string) (attached, rootInforming bool, err error)
}

type Evaluator struct {
	Rules     ruleLister
	Query     *Query
	Incidents *IncidentService
	Notifier  *MetricNotifier
	Interval  time.Duration

	// Окна обслуживания проекта — nil-совместимо: без него просто никогда не подавляет (inMaintenance
	// всегда false), не паникует. Прод (main.go, startEvaluators) обязан заполнять.
	Maint MaintenanceChecker

	// Политика эскалации: резолвит лесенку (project, severity) на открытии. Nil-совместима — без неё
	// просто не уведомляет об открытии.
	Policy *escalation.PolicyStore

	// Та же PG, что под Incidents/Rules: читает лог эскалации для адресного recovery при закрытии.
	// Nil-совместим.
	Pool *pgxpool.Pool

	// Членство свежеоткрытого инцидента правила label_key='host' в группе его down-корня. Уведомление уходит
	// только при «не maintenance И не grouped» (порядок проверок не важен). Nil-совместимо, как Maint.
	IncidentGroups metricGroupHook

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

func (e *Evaluator) Run(ctx context.Context) {
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

func (e *Evaluator) interval() time.Duration {
	if e.Interval <= 0 {
		return evaluatorDefaultInterval
	}
	return e.Interval
}

func (e *Evaluator) tickBudget() time.Duration {
	budget := time.Duration(float64(e.interval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// Self-метрика живости: умерший или отставший оценщик снаружи выглядит ровно как «по всем правилам
// спокойно» — молчание и есть нормальный вывод, как у host.Evaluator/slo.Evaluator.
func (e *Evaluator) LastTickUnix() int64 { return e.lastTickUnix.Load() }

func (e *Evaluator) LastTickSeconds() float64 {
	return math.Float64frombits(e.lastTickSeconds.Load())
}

// Ошибка по одному правилу не роняет остальные (error-isolation). Тик ограничен дедлайном: Query берёт
// голый CH-запрос без своего таймаута — без внешнего дедлайна повисший запрос держал бы тик бесконечно.
func (e *Evaluator) Tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.tickBudget())
	defer cancel()

	rules, err := e.Rules.ListEnabled(ctx)
	if err != nil {
		slog.Error("metric evaluator: list rules failed", "error", err)
		e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		return
	}
	now := time.Now().UTC()
	for _, r := range rules {
		e.evalRule(ctx, r, now)
	}

	e.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("metric evaluator: tick did not finish within its budget",
			"budget", e.tickBudget(), "rules", len(rules))
		return
	}
	e.lastTickUnix.Store(time.Now().Unix())
}

func (e *Evaluator) evalRule(ctx context.Context, r Rule, now time.Time) {
	from := now.Add(-time.Duration(r.WindowSeconds) * time.Second)
	var matchers []LabelMatcher
	if r.LabelKey != "" {
		matchers = []LabelMatcher{{Key: r.LabelKey, Value: r.LabelValue}}
	}
	current, ok, err := e.Query.Aggregate(ctx, r.ProjectID, r.MetricName, r.Environment, "", matchers, r.Aggregation, from, now)
	if err != nil {
		slog.Error("metric evaluator: aggregate failed", "rule_id", r.ID, "error", err)
		return
	}
	if !ok {
		return // нет данных за окно — не открываем и не закрываем
	}

	open, opened, err := e.Incidents.OpenFor(ctx, r.ID)
	if err != nil {
		slog.Error("metric evaluator: open-for failed", "rule_id", r.ID, "error", err)
		return
	}

	d := Decide(current, r.Comparator, r.Threshold, opened)
	switch {
	case d.Open:
		inMaint := e.inMaintenance(ctx, r.ProjectID, now)
		in, created, err := e.Incidents.Open(ctx, r.ID, r.ProjectID, current, inMaint, r.Severity)
		if err != nil {
			slog.Error("metric evaluator: open failed", "rule_id", r.ID, "error", err)
			return
		}
		if created {
			// Членство решается и для инцидента, открытого в maintenance — состав группы собирается всегда,
			// гейтится только уведомление.
			grouped := e.groupGate(ctx, r, in)
			if !inMaint && !grouped {
				e.notifyOpen(ctx, r, in)
			}
		}
	case d.Bump:
		peak := worse(r.Comparator, open.PeakValue, current)
		if err := e.Incidents.Bump(ctx, open.ID, current, peak); err != nil {
			slog.Error("metric evaluator: bump failed", "rule_id", r.ID, "error", err)
		}
	case d.Close:
		ok, err := e.Incidents.Resolve(ctx, open.ID, current)
		if err != nil {
			slog.Error("metric evaluator: resolve failed", "rule_id", r.ID, "error", err)
			return
		}
		if ok {
			e.notifyClose(ctx, open)
		}
	}
}

// override правила, а при его отсутствии ("") — table-DEFAULT metric_incidents.severity ('warning'),
// той же константой, что IncidentService.Open подставляет в БД через COALESCE.
func ruleSeverity(r Rule) string {
	if r.Severity != "" {
		return r.Severity
	}
	return escalation.SeverityWarning
}

// Ошибка проверки НЕ отменяет открытие инцидента — трактуется как «не в окне»: молчать о реальном
// инциденте дороже, чем уведомить лишний раз (как host.Evaluator.inMaintenance). Maint==nil — тот же результат.
func (e *Evaluator) inMaintenance(ctx context.Context, projectID int64, now time.Time) bool {
	if e.Maint == nil {
		return false
	}
	v, err := e.Maint.InMaintenance(ctx, projectID, now)
	if err != nil {
		slog.Error("metric evaluator: maintenance check failed, treating as not in maintenance",
			"project_id", projectID, "error", err)
		return false
	}
	return v
}

// Только правила label_key='host' — их инциденты присоединяются к группе down-корня своего хоста.
// Fail-safe (fail-noisy): ошибка → шумим как без группировки, лучше лишний алерт, чем пропущенный.
func (e *Evaluator) groupGate(ctx context.Context, r Rule, in Incident) bool {
	if e.IncidentGroups == nil || r.LabelKey != "host" || r.LabelValue == "" {
		return false
	}
	attached, informing, err := e.IncidentGroups.AttachMetric(ctx, in.ID, r.ProjectID, r.LabelValue)
	if err != nil {
		slog.Error("metric evaluator: group attach failed", "incident_id", in.ID, "error", err)
		return false
	}
	return attached && informing
}

// Открытие резолвит лесенку эскалации (project, severity правила) и шлёт РОВНО СТУПЕНЬ 0, если её
// задержка уже настала; остальные ступени досылает планировщик. Ошибка не должна ронять оценку.
func (e *Evaluator) notifyOpen(ctx context.Context, r Rule, in Incident) {
	if e.Policy == nil || e.Notifier == nil || e.Pool == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, r.ProjectID, ruleSeverity(r))
	if err != nil {
		slog.Error("metric evaluator: escalation policy failed", "incident_id", in.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", e.Pool, in.ID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return e.Notifier.NotifyStep(ctx, in.ID, chs, step) },
		func(id int64, from int) (bool, error) { return e.Incidents.BumpEscalation(ctx, id, from) })
	if err != nil {
		slog.Error("metric evaluator: notify step failed", "incident_id", in.ID, "error", err)
		return
	}
	if sent {
		if err := e.Incidents.MarkNotified(ctx, in.ID, true); err != nil {
			slog.Error("metric evaluator: mark notified failed", "incident_id", in.ID, "error", err)
		}
	}
}

// Закрытие шлёт recovery адресно, в каналы из лога эскалации — пустой набор значит молчание
// (отправлять «закрыт» нечего).
func (e *Evaluator) notifyClose(ctx context.Context, open Incident) {
	if e.Pool == nil || e.Notifier == nil {
		return
	}
	chs, err := escalation.RecoveryChannels(ctx, e.Pool, "metric", open.ID)
	if err != nil {
		slog.Error("metric evaluator: recovery channels failed", "incident_id", open.ID, "error", err)
		return
	}
	if len(chs) == 0 {
		return
	}
	if err := e.Notifier.NotifyRecovery(ctx, open.ID, chs); err != nil {
		slog.Error("metric evaluator: notify recovery failed", "incident_id", open.ID, "error", err)
		return
	}
	if err := e.Incidents.MarkNotified(ctx, open.ID, false); err != nil {
		slog.Error("metric evaluator: mark notified failed", "incident_id", open.ID, "error", err)
	}
}

// Для gt экстремум — больший, для lt — меньший (peak = худшее значение за инцидент).
func worse(comparator string, a, b float64) float64 {
	if comparator == "lt" {
		if b < a {
			return b
		}
		return a
	}
	if b > a {
		return b
	}
	return a
}
