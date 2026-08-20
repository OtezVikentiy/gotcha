package metric

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

const evaluatorDefaultInterval = 60 * time.Second

// Evaluator периодически считает агрегат каждой enabled-метрики за окно правила
// и открывает/закрывает инциденты, шлёт алерт ровно один раз на открытие и
// закрытие (калька trace.Evaluator). Тикер живёт в режимах uptime|all.
// ruleLister — источник включённых правил. Интерфейс, а не конкретный тип,
// ровно по той же причине, что issueUpserter/eventSink в пакете ingest: без
// него у цикла Run нет наблюдаемого следа, и тест «поспал и убедился, что
// горутина вышла» оставался зелёным, даже если вырезать тело тика целиком.
type ruleLister interface {
	ListEnabled(ctx context.Context) ([]Rule, error)
}

type Evaluator struct {
	Rules     ruleLister
	Query     *Query
	Incidents *IncidentService
	Notifier  *MetricNotifier
	Interval  time.Duration

	// Maint — окна обслуживания проекта (B3: подавление уведомлений). Nil-
	// совместим: деградированная сборка без него просто никогда не подавляет
	// (inMaintenance всегда false), а не паникует. ПРОД (main.go,
	// startEvaluators) обязан его заполнять.
	Maint MaintenanceChecker

	// Policy — политика эскалации (B4, T7): резолвит лесенку (project,
	// severity) на открытии инцидента. Nil-совместим — деградированная сборка
	// без него просто не уведомляет об открытии.
	Policy *escalation.PolicyStore

	// Pool — та же PG, что под Incidents/Rules: читает лог эскалации
	// incident_escalations для адресного recovery при закрытии (B4, T7, см.
	// notifyClose, escalation.RecoveryChannels). Nil-совместим.
	Pool *pgxpool.Pool
}

// Run тикает каждый Interval, пока не отменят ctx.
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
			e.Tick(ctx)
		}
	}
}

// Tick — один проход по всем enabled-правилам. Ошибка по одному правилу не
// роняет остальные (error-isolation).
func (e *Evaluator) Tick(ctx context.Context) {
	rules, err := e.Rules.ListEnabled(ctx)
	if err != nil {
		slog.Error("metric evaluator: list rules failed", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, r := range rules {
		e.evalRule(ctx, r, now)
	}
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
		if created && !inMaint {
			e.notifyOpen(ctx, r, in)
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
		if ok && !open.InMaintenance {
			e.notifyClose(ctx, open)
		}
	}
}

// ruleSeverity — severity для резолва лесенки эскалации: override правила
// (Task 5, metric_alert_rules.severity), а "" (нет override) — table-DEFAULT
// metric_incidents.severity ('warning', 0077), той же константой, что
// IncidentService.Open подставляет в БД через COALESCE.
func ruleSeverity(r Rule) string {
	if r.Severity != "" {
		return r.Severity
	}
	return escalation.SeverityWarning
}

// inMaintenance — проект сейчас в окне обслуживания (B3), для гейта
// open-notify в evalRule. Ошибка проверки НЕ отменяет открытие инцидента: она
// лишь означает, что не удалось выяснить, плановые ли это работы, и
// трактуется как «не в окне» — молчать о реальном инциденте дороже, чем
// уведомить лишний раз (то же решение, что host.Evaluator.inMaintenance).
// Maint==nil (деградированная сборка) — тот же результат.
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

// notifyOpen — реролл (B4, T7): открытие инцидента резолвит лесенку
// эскалации (project, severity правила — ruleSeverity) и шлёт РОВНО СТУПЕНЬ
// 0, если её задержка (обычно 0) уже настала; остальные ступени досылает
// планировщик (T8). Ошибка политики/уведомления не должна ронять оценку.
func (e *Evaluator) notifyOpen(ctx context.Context, r Rule, in Incident) {
	if e.Policy == nil || e.Notifier == nil {
		return
	}
	ladder, err := e.Policy.Ladder(ctx, r.ProjectID, ruleSeverity(r))
	if err != nil {
		slog.Error("metric evaluator: escalation policy failed", "incident_id", in.ID, "error", err)
		return
	}
	sent, err := escalation.SendStepIfDue(ctx, ladder, in.ID, 0, 0,
		func(chs []int64, step int) error { return e.Notifier.NotifyStep(ctx, in.ID, chs, step) },
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

// notifyClose — реролл (B4, T7): закрытие инцидента шлёт recovery адресно, в
// каналы из лога эскалации (escalation.RecoveryChannels); пустой набор —
// молчание (M-7 брифа Task 6, ничего не отправлялось — отправлять «закрыт»
// нечего).
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

// worse возвращает экстремум в сторону нарушения: для gt — больший, для lt —
// меньший (peak = самое «плохое» значение за время инцидента).
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
