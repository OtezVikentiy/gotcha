package metric

import (
	"context"
	"log/slog"
	"time"
)

const evaluatorDefaultInterval = 60 * time.Second

// Evaluator периодически считает агрегат каждой enabled-метрики за окно правила
// и открывает/закрывает инциденты, шлёт алерт ровно один раз на открытие и
// закрытие (калька trace.Evaluator). Тикер живёт в режимах uptime|all.
type Evaluator struct {
	Rules     *RuleService
	Query     *Query
	Incidents *IncidentService
	Notifier  *MetricNotifier
	Interval  time.Duration
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
	matcher := LabelMatcher{Key: r.LabelKey, Value: r.LabelValue}
	current, ok, err := e.Query.Aggregate(ctx, r.ProjectID, r.MetricName, r.Environment, matcher, r.Aggregation, from, now)
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
		in, created, err := e.Incidents.Open(ctx, r.ID, r.ProjectID, current)
		if err != nil {
			slog.Error("metric evaluator: open failed", "rule_id", r.ID, "error", err)
			return
		}
		if created {
			e.notify(ctx, r, in, current, current, true)
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
			e.notify(ctx, r, open, current, open.PeakValue, false)
		}
	}
}

// notify шлёт событие и помечает инцидент, чтобы алерт ушёл ровно один раз.
func (e *Evaluator) notify(ctx context.Context, r Rule, in Incident, current, peak float64, opened bool) {
	if e.Notifier == nil {
		return
	}
	ev := MetricEvent{
		ProjectID: r.ProjectID, RuleID: r.ID, MetricName: r.MetricName, Aggregation: r.Aggregation,
		Comparator: r.Comparator, Threshold: r.Threshold, Current: current, Peak: peak,
		Environment: r.Environment, LabelKey: r.LabelKey, LabelValue: r.LabelValue, Opened: opened,
	}
	if err := e.Notifier.Notify(ctx, ev); err != nil {
		slog.Error("metric evaluator: notify failed", "rule_id", r.ID, "error", err)
	}
	if err := e.Incidents.MarkNotified(ctx, in.ID, opened); err != nil {
		slog.Error("metric evaluator: mark notified failed", "incident_id", in.ID, "error", err)
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
