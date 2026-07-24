package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// RegressionEvent — открытие или закрытие инцидента-регрессии производительности
// (см. RegressionService). Kind различает событие; для close заполнен
// DurationSeconds (сколько инцидент был открыт).
type RegressionEvent struct {
	Kind            string // "regression_open" | "regression_close"
	ProjectID       int64
	Target          string // имя цели: эндпойнт или страница vital
	Metric          string // duration | lcp | inp | cls | fcp | ttfb
	BaselineValue   float64
	CurrentValue    float64
	PctIncrease     float64 // (current-base)/base — доля, не проценты
	DurationSeconds int64   // для close: сколько инцидент был открыт
}

// RegressionNotifier — алерты об открытии/закрытии регрессий поверх того же
// notify.Outbox и тех же каналов проекта, что и алерты uptime
// (uptime.OutboxNotifier) и perf-issues (trace.OutboxNotifier): формат payload
// намеренно совпадает с ними — обязательные channel_kind/target/secret читает
// notify.Worker, доставляют те же Sender'ы.
type RegressionNotifier struct {
	Alerts *alert.Service // каналы проекта: Alerts.Channels(projectID)
	Outbox *notify.Outbox

	// BaseURL — префикс ссылки на список регрессий проекта в уведомлении:
	// {BaseURL}/projects/{project_id}/regressions.
	BaseURL string

	// EmailEnabled — см. alert.Evaluator.EmailEnabled: пока false,
	// email-каналы пропускаются (с warn-логом), чтобы не ставить в очередь
	// задачи, которые notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// ExternalDetails — см. alert.Evaluator.ExternalDetails: при false во
	// внешние каналы (Telegram/webhook) уходит обезличенный payload без имени
	// цели регрессии/метрики и значений (потенциальные ПДн за пределами РФ,
	// 152-ФЗ). Дефолт — false (обезличенный); true — явное включение
	// оператором (GOTCHA_EXTERNAL_CHANNEL_DETAILS).
	ExternalDetails bool
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает постановку остальных: все такие
// ошибки логируются и собираются через errors.Join (как в uptime.OutboxNotifier).
// Проект без включённых каналов — не ошибка: задач просто не будет.
func (n *RegressionNotifier) Notify(ctx context.Context, ev RegressionEvent) error {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return fmt.Errorf("trace: regression notify: project channels: %w", err)
	}

	url := fmt.Sprintf("%s/projects/%d/regressions", n.BaseURL, ev.ProjectID)
	subject := regressionSubject(ev)
	body := regressionBody(ev, url)

	var errs error
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("trace: regression email channel skipped, SMTP not configured",
				"project_id", ev.ProjectID, "channel_id", ch.ID)
			continue
		}

		// Ловушка имён: адрес канала (webhook URL / chat_id) кладём под "target"
		// — его читает notify.Worker; имя цели регрессии — под "target_name".
		payload := map[string]any{
			"kind":           ev.Kind,
			"project_id":     ev.ProjectID,
			"target_name":    ev.Target,
			"metric":         ev.Metric,
			"baseline_value": ev.BaselineValue,
			"current_value":  ev.CurrentValue,
			"pct_increase":   ev.PctIncrease,
			"url":            url,
			"subject":        subject,
			"body":           body,
			"channel_kind":   ch.Kind,
			"target":         ch.Target,
			"secret":         ch.Secret,
		}
		// Гейт трансграничной передачи: во внешние каналы без ExternalDetails
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.ExternalDetails && (ch.Kind == alert.ChannelTelegram || ch.Kind == alert.ChannelWebhook) {
			payload = notify.RedactExternalPayload(payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("trace: regression notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("trace: regression notify: enqueue channel %d: %w", ch.ID, err))
		}
	}
	return errs
}

// regressionSubject строит тему уведомления по виду события.
func regressionSubject(ev RegressionEvent) string {
	switch ev.Kind {
	case "regression_close":
		return fmt.Sprintf("[Gotcha] Регрессия устранена: %s %s (%s)",
			ev.Target, ev.Metric, formatDuration(ev.DurationSeconds))
	default: // regression_open
		return fmt.Sprintf("[Gotcha] Регрессия: %s %s +%s%% (%s → %s)",
			ev.Target, ev.Metric, formatPct(ev.PctIncrease),
			formatMetric(ev.Metric, ev.BaselineValue), formatMetric(ev.Metric, ev.CurrentValue))
	}
}

// regressionBody строит человекочитаемый текст уведомления: цель, метрика,
// база/текущее — плюс ссылка на список регрессий.
func regressionBody(ev RegressionEvent, url string) string {
	base := formatMetric(ev.Metric, ev.BaselineValue)
	cur := formatMetric(ev.Metric, ev.CurrentValue)
	switch ev.Kind {
	case "regression_close":
		return fmt.Sprintf("Регрессия устранена: %s %s.\n\nБыло: %s\nСтало: %s\nДлительность: %s\n\n%s",
			ev.Target, ev.Metric, base, cur, formatDuration(ev.DurationSeconds), url)
	default: // regression_open
		return fmt.Sprintf("Обнаружена регрессия: %s %s вырос на %s%%.\n\nБаза: %s\nСейчас: %s\n\n%s",
			ev.Target, ev.Metric, formatPct(ev.PctIncrease), base, cur, url)
	}
}

// formatPct отображает долю (current-base)/base целым числом процентов:
// 0.5 → "50", 1.5 → "150".
func formatPct(ratio float64) string {
	return fmt.Sprintf("%.0f", ratio*100)
}

// formatMetric отображает значение метрики человекочитаемо: cls — безразмерное
// отношение (0.25), остальные (duration/lcp/inp/fcp/ttfb) — время в мс, а от
// секунды и выше — в секундах (1200ms → "1.2s").
func formatMetric(metric string, v float64) string {
	if metric == "cls" {
		return fmt.Sprintf("%.2f", v)
	}
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", v/1000)
	}
	return fmt.Sprintf("%.0fms", v)
}

// formatDuration отображает секунды в компактном человекочитаемом виде:
// "45s" (< 1 минуты), "2m5s" (< 1 часа) или "1h5m" (>= 1 часа, секунды
// отбрасываются как незначимые на таком масштабе). Совпадает с
// uptime.formatDuration — держим свою копию, чтобы не тянуть зависимость на пакет.
func formatDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	h := int64(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int64(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int64(d / time.Second)

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
