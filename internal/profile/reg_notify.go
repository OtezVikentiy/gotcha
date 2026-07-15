package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
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
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает остальные (errors.Join).
func (n *RegressionNotifier) Notify(ctx context.Context, ev ProfileRegressionEvent) error {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return fmt.Errorf("profile: regression notify: project channels: %w", err)
	}
	url := fmt.Sprintf("%s/projects/%d/profile-regressions", n.BaseURL, ev.ProjectID)
	subject := regressionSubject(ev)
	body := regressionBody(ev, url)

	var errs error
	for _, ch := range channels {
		if !ch.Enabled {
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
			"secret":         ch.Secret,
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

func regressionSubject(ev ProfileRegressionEvent) string {
	if ev.Opened {
		return fmt.Sprintf("[gotcha] profile regression: %s +%s%%", ev.Function, formatPct(ev.PctIncrease))
	}
	return fmt.Sprintf("[gotcha] profile regression resolved: %s", ev.Function)
}

func regressionBody(ev ProfileRegressionEvent, url string) string {
	head := "Profile regression resolved."
	if ev.Opened {
		head = "A function's self-CPU share regressed."
	}
	return fmt.Sprintf("%s\nFunction: %s\nService: %s (%s)\nSelf-CPU share: %s%% → %s%% (+%s%%)\n%s",
		head, ev.Function, ev.Service, ev.ProfileType,
		formatShare(ev.BaselineShare), formatShare(ev.CurrentShare), formatPct(ev.PctIncrease), url)
}

// formatPct — доля роста в процентах (0.5 → "50").
func formatPct(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 0, 64)
}

// formatShare — доля (0.2 → "20.0") в процентах.
func formatShare(share float64) string {
	return strconv.FormatFloat(share*100, 'f', 1, 64)
}
