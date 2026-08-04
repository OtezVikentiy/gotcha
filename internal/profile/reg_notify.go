package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// ProfileRegressionEvent — открытие/закрытие инцидента регрессии self-CPU функции.
type ProfileRegressionEvent struct {
	ProjectID     int64
	Service       string
	ProfileType   string
	Function      string
	BaselineShare float64
	CurrentShare  float64
	PctIncrease   float64 // доля роста (0.5 = +50%); форматтер ×100
	Opened        bool
}

// RegressionNotifier ставит уведомления о регрессиях профилей в общий Outbox по
// каналам проекта (калька trace.RegressionNotifier / metric.MetricNotifier).
type RegressionNotifier struct {
	Alerts       *alert.Service
	Outbox       *notify.Outbox
	BaseURL      string
	EmailEnabled bool

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. alert.DetailPolicy). Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык уведомления выбирает оператор (класс №133–136).
	Locale i18n.Locale
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает остальные (errors.Join).
func (n *RegressionNotifier) Notify(ctx context.Context, ev ProfileRegressionEvent) error {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return fmt.Errorf("profile: regression notify: project channels: %w", err)
	}
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)
	url := fmt.Sprintf("%s/projects/%d/profile-regressions", n.BaseURL, ev.ProjectID)
	subject := regressionSubject(ctx, ev)
	body := regressionBody(ctx, ev, url)

	var errs error
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("profile: regression email channel skipped, SMTP not configured",
				"project_id", ev.ProjectID, "channel_id", ch.ID)
			continue
		}
		payload := map[string]any{
			"kind":           regressionKind(ev),
			"project_id":     ev.ProjectID,
			"service":        ev.Service,
			"profile_type":   ev.ProfileType,
			"function":       ev.Function,
			"baseline_share": ev.BaselineShare,
			"current_share":  ev.CurrentShare,
			"pct_increase":   ev.PctIncrease,
			"url":            url,
			"subject":        subject,
			"body":           body,
			"channel_kind":   ch.Kind,
			"target":         ch.Target,
			// Секрета в payload нет намеренно: notification_outbox.payload —
			// обычный jsonb, и bot-токен в нём обесценил бы шифрование
			// alert_channels.secret. notify.Worker достаёт секрет по
			// channel_id в момент отправки (см. notify.SecretResolver).
		}
		// Гейт трансграничной передачи: получателю вне контура оператора
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.Details.AllowsDetails(ch) {
			payload = notify.RedactExternalPayload(ctx, payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("profile: regression notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("profile: regression notify: enqueue channel %d: %w", ch.ID, err))
		}
	}
	return errs
}

func regressionKind(ev ProfileRegressionEvent) string {
	if ev.Opened {
		return "profile_regression_open"
	}
	return "profile_regression_resolved"
}

// regressionSubject / regressionBody строят тексты из каталога i18n — по
// локали, положенной в ctx (класс №133–136: язык внешнего канала задаёт
// GOTCHA_LOCALE, см. RegressionNotifier.Locale).
func regressionSubject(ctx context.Context, ev ProfileRegressionEvent) string {
	if ev.Opened {
		return i18n.Tf(ctx, "notify.profile.subject.open",
			"function", ev.Function, "percent", formatPct(ev.PctIncrease))
	}
	return i18n.Tf(ctx, "notify.profile.subject.close", "function", ev.Function)
}

func regressionBody(ctx context.Context, ev ProfileRegressionEvent, url string) string {
	key := "notify.profile.body.close"
	if ev.Opened {
		key = "notify.profile.body.open"
	}
	return i18n.Tf(ctx, key,
		"function", ev.Function, "service", ev.Service, "type", ev.ProfileType,
		"base", formatShare(ev.BaselineShare), "current", formatShare(ev.CurrentShare),
		"percent", formatPct(ev.PctIncrease), "url", url)
}

// formatPct — доля роста в процентах (0.5 → "50").
func formatPct(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 0, 64)
}

// formatShare — доля (0.2 → "20.0") в процентах.
func formatShare(share float64) string {
	return strconv.FormatFloat(share*100, 'f', 1, 64)
}
