package metric

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// MetricEvent — открытие или закрытие инцидента порогового алерта на метрику.
type MetricEvent struct {
	ProjectID   int64
	RuleID      int64
	MetricName  string
	Aggregation string
	Comparator  string // 'gt' | 'lt'
	Threshold   float64
	Current     float64
	Peak        float64
	Environment string
	LabelKey    string
	LabelValue  string
	Opened      bool // true — открытие, false — закрытие
}

// MetricNotifier ставит уведомления об инцидентах метрик в общий Outbox по
// каналам проекта (калька trace.RegressionNotifier).
type MetricNotifier struct {
	Alerts       *alert.Service
	Outbox       *notify.Outbox
	BaseURL      string
	EmailEnabled bool

	// ExternalDetails — см. alert.Evaluator.ExternalDetails: при false во
	// внешние каналы (Telegram/webhook) уходит обезличенный payload без имени
	// метрики и значений (потенциально чувствительны за пределами РФ, 152-ФЗ).
	// true (дефолт из cfg) — поведение прежнее.
	ExternalDetails bool
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает остальные (errors.Join). Проект
// без каналов — не ошибка.
func (n *MetricNotifier) Notify(ctx context.Context, ev MetricEvent) error {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return fmt.Errorf("metric: notify: project channels: %w", err)
	}
	url := fmt.Sprintf("%s/projects/%d/metrics/alerts", n.BaseURL, ev.ProjectID)
	subject := metricSubject(ev)
	body := metricBody(ev, url)

	var errs error
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("metric: email channel skipped, SMTP not configured",
				"project_id", ev.ProjectID, "channel_id", ch.ID)
			continue
		}
		payload := map[string]any{
			"kind":          metricEventKind(ev),
			"project_id":    ev.ProjectID,
			"metric":        ev.MetricName,
			"aggregation":   ev.Aggregation,
			"comparator":    ev.Comparator,
			"threshold":     ev.Threshold,
			"current_value": ev.Current,
			"peak_value":    ev.Peak,
			"url":           url,
			"subject":       subject,
			"body":          body,
			// Адрес/секрет канала — их читает notify.Worker (та же «ловушка имён»,
			// что в trace.RegressionNotifier).
			"channel_kind": ch.Kind,
			"target":       ch.Target,
			"secret":       ch.Secret,
		}
		// Гейт трансграничной передачи: во внешние каналы без ExternalDetails
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.ExternalDetails && (ch.Kind == alert.ChannelTelegram || ch.Kind == alert.ChannelWebhook) {
			payload = notify.RedactExternalPayload(payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("metric: notify enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("metric: notify: enqueue channel %d: %w", ch.ID, err))
		}
	}
	return errs
}

func metricEventKind(ev MetricEvent) string {
	if ev.Opened {
		return "metric_alert_open"
	}
	return "metric_alert_resolved"
}

func metricSubject(ev MetricEvent) string {
	state := "resolved"
	if ev.Opened {
		state = "firing"
	}
	return fmt.Sprintf("[gotcha] metric %s %s %s %s (%s)",
		ev.MetricName, ev.Aggregation, cmpSymbol(ev.Comparator), formatNum(ev.Threshold), state)
}

func metricBody(ev MetricEvent, url string) string {
	scope := ev.Environment
	if scope == "" {
		scope = "all environments"
	}
	if ev.LabelKey != "" {
		scope += fmt.Sprintf(", %s=%s", ev.LabelKey, ev.LabelValue)
	}
	head := "Metric threshold resolved."
	if ev.Opened {
		head = "Metric threshold breached."
	}
	return fmt.Sprintf("%s\nMetric: %s (%s)\nCondition: %s %s %s\nCurrent: %s (peak %s)\nScope: %s\n%s",
		head, ev.MetricName, ev.Aggregation,
		ev.Aggregation, cmpSymbol(ev.Comparator), formatNum(ev.Threshold),
		formatNum(ev.Current), formatNum(ev.Peak), scope, url)
}

func cmpSymbol(comparator string) string {
	if comparator == "lt" {
		return "<"
	}
	return ">"
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}
