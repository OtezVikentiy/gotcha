package profile

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

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

type RegressionNotifier struct {
	Alerts       *alert.Service
	Outbox       *notify.Outbox
	BaseURL      string
	EmailEnabled bool

	// нулевое значение политики не раскрывает детали никому.
	Details alert.DetailPolicy

	// язык из GOTCHA_LOCALE: внешний канал не знает локали получателя, её выбирает оператор.
	Locale i18n.Locale

	// нужен для перезагрузки регрессии по ID: NotifyStep/NotifyRecovery получают
	// только incidentID, без готового события.
	Regressions *RegressionService

	Pool *pgxpool.Pool

	// nil-совместим: без него уведомления идут без имени проекта.
	Projects escalation.ProjectNamer
}

// ошибка Enqueue по одному каналу не прерывает остальные (errors.Join).
func (n *RegressionNotifier) Notify(ctx context.Context, ev ProfileRegressionEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// лог incident_escalations пишет вызывающая оркестрация, не сам NotifyStep.
// channelIDs nil/пусто — все deliverable-каналы проекта.
func (n *RegressionNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	r, ok, err := n.Regressions.GetByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("profile: notify step: load regression: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("profile: notify step: regression %d not found", incidentID)
	}
	ev := profileRegressionEvent(r, true)
	return n.dispatch(ctx, ev, channelIDs)
}

// recovery не эскалирует — не логируется вообще; channelIDs nil/пусто — все
// deliverable-каналы проекта.
func (n *RegressionNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	r, ok, err := n.Regressions.GetByID(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("profile: notify recovery: load regression: %w", err)
	}
	if !ok {
		return fmt.Errorf("profile: notify recovery: regression %d not found", incidentID)
	}
	ev := profileRegressionEvent(r, false)
	_, err = n.dispatch(ctx, ev, channelIDs)
	return err
}

// доля роста пересчитывается из baseline/current тем же pctIncrease, что и
// оценщик, а не хранится отдельно.
func profileRegressionEvent(r Regression, opened bool) ProfileRegressionEvent {
	return ProfileRegressionEvent{
		ProjectID:     r.ProjectID,
		Service:       r.Service,
		ProfileType:   r.ProfileType,
		Function:      r.Function,
		BaselineShare: r.BaselineShare,
		CurrentShare:  r.CurrentShare,
		PctIncrease:   pctIncrease(r.BaselineShare, r.CurrentShare),
		Opened:        opened,
	}
}

// channelIDs nil/пусто — все deliverable-каналы проекта; непустой — фильтр по членству после гейта.
// Логировать возвращённые каналы в incident_escalations или нет — решает вызывающий, не dispatch.
func (n *RegressionNotifier) dispatch(ctx context.Context, ev ProfileRegressionEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("profile: regression notify: project channels: %w", err)
	}
	// язык инстанса, а не запроса: читает внешний получатель без своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)
	url := fmt.Sprintf("%s/projects/%d/profile-regressions", n.BaseURL, ev.ProjectID)
	subject := regressionSubject(ctx, ev)
	body := regressionBody(ctx, ev, url)

	dchans := make([]escalation.DispatchChannel, 0, len(channels))
	for _, ch := range channels {
		dchans = append(dchans, escalation.DispatchChannel{
			ID: ch.ID, Kind: ch.Kind, Target: ch.Target,
			IsEmail:       ch.Kind == alert.ChannelEmail,
			Deliverable:   ch.Deliverable(),
			AllowsDetails: n.Details.AllowsDetails(ch),
		})
	}

	kind := notify.KindProfileRegressionResolved
	if ev.Opened {
		kind = notify.KindProfileRegressionOpen
	}

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "profile"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: kind, Subject: subject, Body: body,
			URL: url,
			Extra: map[string]any{
				"service":        ev.Service,
				"profile_type":   ev.ProfileType,
				"function":       ev.Function,
				"baseline_share": ev.BaselineShare,
				"current_share":  ev.CurrentShare,
				"pct_increase":   ev.PctIncrease,
			},
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

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

// доля → проценты, без десятичных (0.5 → "50").
func formatPct(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 0, 64)
}

// доля → проценты с одним знаком после запятой (0.2 → "20.0").
func formatShare(share float64) string {
	return strconv.FormatFloat(share*100, 'f', 1, 64)
}
