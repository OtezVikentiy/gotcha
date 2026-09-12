package trace

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

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

type RegressionNotifier struct {
	Alerts *alert.Service
	Outbox *notify.Outbox

	// ссылка на список регрессий проекта: значение + /projects/{project_id}/regressions.
	BaseURL string

	EmailEnabled bool

	// нулевое значение не раскрывает деталей никому.
	Details alert.DetailPolicy

	// локаль инстанса (GOTCHA_LOCALE), не запроса: у внешнего получателя своей локали нет.
	Locale i18n.Locale

	// нужен, чтобы перезагрузить регрессию по id — у NotifyStep/NotifyRecovery
	// нет готового RegressionEvent на входе, как у Notify.
	Regressions *RegressionService

	// пишет лог эскалации incident_escalations после каждого успешного Enqueue в NotifyStep.
	Pool *pgxpool.Pool

	// nil-совместим — тогда уведомления идут без имени проекта.
	Projects escalation.ProjectNamer
}

// ошибка Enqueue по одному каналу не прерывает остальные; проект без
// включённых каналов — не ошибка, просто нет задач.
func (n *RegressionNotifier) Notify(ctx context.Context, ev RegressionEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// лог incident_escalations пишет вызывающий (escalation.SendStepIfDue), не
// этот метод — иначе с мок-нотифаером тестов лог молчал бы.
func (n *RegressionNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	r, ok, err := n.Regressions.GetByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("trace: notify step: load regression: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("trace: notify step: regression %d not found", incidentID)
	}
	ev := regressionOpenEvent(r)
	return n.dispatch(ctx, ev, channelIDs)
}

// в отличие от NotifyStep, recovery не логируется в incident_escalations.
func (n *RegressionNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	r, ok, err := n.Regressions.GetByID(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("trace: notify recovery: load regression: %w", err)
	}
	if !ok {
		return fmt.Errorf("trace: notify recovery: regression %d not found", incidentID)
	}
	ev := regressionCloseEvent(r, time.Now())
	_, err = n.dispatch(ctx, ev, channelIDs)
	return err
}

// PctIncrease пересчитывается из baseline/current, не хранится в таблице.
func regressionOpenEvent(r Regression) RegressionEvent {
	return RegressionEvent{
		Kind:          "regression_open",
		ProjectID:     r.ProjectID,
		Target:        r.Target,
		Metric:        r.Metric,
		BaselineValue: r.BaselineValue,
		CurrentValue:  r.CurrentValue,
		PctIncrease:   pctIncrease(r.BaselineValue, r.CurrentValue),
	}
}

// duration — до ResolvedAt, а если он ещё не проставлен (recovery позвал
// раньше Resolve) — до now.
func regressionCloseEvent(r Regression, now time.Time) RegressionEvent {
	end := now
	if r.ResolvedAt != nil {
		end = *r.ResolvedAt
	}
	d := end.Sub(r.StartedAt)
	if d < 0 {
		d = 0
	}
	return RegressionEvent{
		Kind:            "regression_close",
		ProjectID:       r.ProjectID,
		Target:          r.Target,
		Metric:          r.Metric,
		BaselineValue:   r.BaselineValue,
		CurrentValue:    r.CurrentValue,
		PctIncrease:     pctIncrease(r.BaselineValue, r.CurrentValue),
		DurationSeconds: int64(d.Seconds()),
	}
}

// channelIDs пустой — все deliverable-каналы (как у Notify); непустой —
// фильтр по членству ПОСЛЕ Deliverable/email-гейта, не вместо него.
func (n *RegressionNotifier) dispatch(ctx context.Context, ev RegressionEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("trace: regression notify: project channels: %w", err)
	}
	ctx = i18n.WithLocale(ctx, n.Locale)

	url := fmt.Sprintf("%s/projects/%d/regressions", n.BaseURL, ev.ProjectID)
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

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "trace"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: ev.Kind, Subject: subject, Body: body,
			URL: url,
			// адрес канала уходит под "target" (собирает сам Dispatch, читает
			// notify.Worker); имя цели регрессии — под "target_name", не "target".
			Extra: map[string]any{
				"target_name":    ev.Target,
				"metric":         ev.Metric,
				"baseline_value": ev.BaselineValue,
				"current_value":  ev.CurrentValue,
				"pct_increase":   ev.PctIncrease,
			},
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

// тот же ctx, что задаёт каталог i18n, определяет и форматирование в humanize.MetricValue.
func regressionSubject(ctx context.Context, ev RegressionEvent) string {
	switch ev.Kind {
	case "regression_close":
		return i18n.Tf(ctx, "notify.regression.subject.close",
			"target", ev.Target, "metric", ev.Metric,
			"duration", formatDuration(ev.DurationSeconds))
	default: // regression_open
		return i18n.Tf(ctx, "notify.regression.subject.open",
			"target", ev.Target, "metric", ev.Metric,
			"percent", formatPct(ev.PctIncrease),
			"base", humanize.MetricValue(ctx, ev.Metric, ev.BaselineValue),
			"current", humanize.MetricValue(ctx, ev.Metric, ev.CurrentValue))
	}
}

func regressionBody(ctx context.Context, ev RegressionEvent, url string) string {
	base := humanize.MetricValue(ctx, ev.Metric, ev.BaselineValue)
	cur := humanize.MetricValue(ctx, ev.Metric, ev.CurrentValue)
	switch ev.Kind {
	case "regression_close":
		return i18n.Tf(ctx, "notify.regression.body.close",
			"target", ev.Target, "metric", ev.Metric, "base", base, "current", cur,
			"duration", formatDuration(ev.DurationSeconds), "url", url)
	default: // regression_open
		return i18n.Tf(ctx, "notify.regression.body.open",
			"target", ev.Target, "metric", ev.Metric,
			"percent", formatPct(ev.PctIncrease), "base", base, "current", cur, "url", url)
	}
}

// 0.5 → "50", 1.5 → "150" — округление до целых процентов.
func formatPct(ratio float64) string {
	return fmt.Sprintf("%.0f", ratio*100)
}

// "45s" / "2m5s" / "1h5m"; совпадает с uptime.formatDuration — держим копию,
// чтобы не тянуть на него зависимость.
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
