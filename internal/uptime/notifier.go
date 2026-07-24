package uptime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// OutboxNotifier — реализация Notifier поверх notify.Outbox: доставляет
// Event, ставя по одной задаче на каждый включённый канал, точно так же,
// как alert.Evaluator делает это для issue-алертов (см. evaluator.go —
// формат payload и правило пропуска email намеренно совпадают).
type OutboxNotifier struct {
	Alerts *alert.Service // для Alerts.Channels(projectID) — фолбэк, если у монитора нет своих каналов
	Uptime *Service       // для Uptime.MonitorChannels(monitorID)
	Outbox *notify.Outbox

	// BaseURL — префикс для ссылки на монитор в уведомлении:
	// {BaseURL}/monitors/{id}.
	BaseURL string

	// EmailEnabled — см. alert.Evaluator.EmailEnabled: пока false,
	// email-каналы пропускаются (с warn-логом), чтобы не ставить в очередь
	// задачи, которые notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// ExternalDetails — см. alert.Evaluator.ExternalDetails: при false во
	// внешние каналы (Telegram/webhook) уходит обезличенный payload без имени
	// монитора и причины падения (потенциально чувствительны за пределами РФ,
	// 152-ФЗ). Дефолт — false (обезличенный); true — явное включение
	// оператором (GOTCHA_EXTERNAL_CHANNEL_DETAILS).
	ExternalDetails bool
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал
// монитора — если у монитора нет своих каналов, используются все
// включённые каналы проекта. Ошибка Enqueue по одному каналу не прерывает
// постановку остальных: все такие ошибки логируются и собираются через
// errors.Join в возвращаемое значение.
func (n *OutboxNotifier) Notify(ctx context.Context, ev Event) error {
	channels, err := n.Uptime.MonitorChannels(ctx, ev.Monitor.ID)
	if err != nil {
		return fmt.Errorf("uptime: notify: monitor channels: %w", err)
	}
	if len(channels) == 0 {
		channels, err = n.Alerts.Channels(ctx, ev.Monitor.ProjectID)
		if err != nil {
			return fmt.Errorf("uptime: notify: project channels: %w", err)
		}
	}

	url := fmt.Sprintf("%s/monitors/%d", n.BaseURL, ev.Monitor.ID)
	subject := subjectFor(ev)
	body := bodyFor(ev, url)

	var errs error
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("uptime: email channel skipped, SMTP not configured",
				"monitor_id", ev.Monitor.ID, "channel_id", ch.ID)
			continue
		}

		payload := map[string]any{
			"kind":             ev.Kind,
			"monitor_id":       ev.Monitor.ID,
			"monitor_name":     ev.Monitor.Name,
			"project_id":       ev.Monitor.ProjectID,
			"regions":          ev.Regions,
			"cause":            ev.Cause,
			"duration_seconds": ev.DurationSeconds,
			"days_left":        ev.DaysLeft,
			"url":              url,
			"subject":          subject,
			"body":             body,
			"channel_kind":     ch.Kind,
			"target":           ch.Target,
			"secret":           ch.Secret,
		}
		// Гейт трансграничной передачи: во внешние каналы без ExternalDetails
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.ExternalDetails && (ch.Kind == alert.ChannelTelegram || ch.Kind == alert.ChannelWebhook) {
			payload = notify.RedactExternalPayload(payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("uptime: notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("uptime: notify: enqueue channel %d: %w", ch.ID, err))
		}
	}
	return errs
}

// subjectFor строит тему письма/сообщения по виду события.
func subjectFor(ev Event) string {
	name := ev.Monitor.Name
	switch ev.Kind {
	case "down":
		return fmt.Sprintf("[Gotcha] %s is DOWN", name)
	case "up":
		return fmt.Sprintf("[Gotcha] %s is back UP (%s)", name, formatDuration(ev.DurationSeconds))
	case "ssl_expiring":
		return fmt.Sprintf("[Gotcha] SSL for %s expires in %d days", name, ev.DaysLeft)
	case "reminder":
		return fmt.Sprintf("[Gotcha] %s still DOWN (%s)", name, formatDuration(ev.DurationSeconds))
	default:
		return fmt.Sprintf("[Gotcha] %s: %s", name, ev.Kind)
	}
}

// bodyFor строит человекочитаемый текст уведомления: причина, регионы,
// время — плюс ссылка на монитор.
func bodyFor(ev Event, url string) string {
	name := ev.Monitor.Name
	regions := strings.Join(ev.Regions, ", ")
	switch ev.Kind {
	case "down":
		return fmt.Sprintf("%s is DOWN.\n\nCause: %s\nRegions: %s\n\n%s", name, ev.Cause, regions, url)
	case "up":
		return fmt.Sprintf("%s is back UP after %s of downtime.\n\n%s", name, formatDuration(ev.DurationSeconds), url)
	case "ssl_expiring":
		return fmt.Sprintf("SSL certificate for %s expires in %d days.\n\n%s", name, ev.DaysLeft, url)
	case "reminder":
		return fmt.Sprintf("%s is still DOWN (%s so far).\n\nCause: %s\nRegions: %s\n\n%s",
			name, formatDuration(ev.DurationSeconds), ev.Cause, regions, url)
	default:
		return fmt.Sprintf("%s\n\n%s", name, url)
	}
}

// formatDuration отображает секунды в компактном человекочитаемом виде:
// "45s" (< 1 минуты), "2m5s" (< 1 часа) или "1h5m" (>= 1 часа, секунды
// отбрасываются как незначимые на таком масштабе).
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
