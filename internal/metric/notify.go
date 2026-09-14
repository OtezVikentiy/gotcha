package metric

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
	Opened      bool
}

type MetricNotifier struct {
	Alerts       *alert.Service
	Outbox       *notify.Outbox
	BaseURL      string
	EmailEnabled bool

	// Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка получателя,
	// поэтому язык уведомления выбирает оператор.
	Locale i18n.Locale

	// Источники перезагрузки инцидента по ID: планировщику эскалации известен
	// только incidentID, готового MetricEvent, как у Notify, нет.
	Incidents *IncidentService
	Rules     *RuleService

	// Та же PG, что у Incidents/Rules/Alerts/Outbox: пишет incident_escalations
	// (миграция 0077) после каждого Enqueue в NotifyStep.
	Pool *pgxpool.Pool

	// nil-совместим: без Projects уведомления идут без имени проекта.
	Projects escalation.ProjectNamer
}

// Ошибка одного канала не прерывает остальные; проект без каналов — не ошибка.
func (n *MetricNotifier) Notify(ctx context.Context, ev MetricEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// channelIDs nil/пусто — все deliverable-каналы проекта. Лог incident_escalations
// пишет вызывающий (SendStepIfDue), не этот метод.
func (n *MetricNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	ev, err := n.reloadEvent(ctx, incidentID, true)
	if err != nil {
		return nil, fmt.Errorf("metric: notify step: %w", err)
	}
	return n.dispatch(ctx, ev, channelIDs)
}

// Не логируется — recovery не эскалирует. channelIDs nil/пусто — все
// deliverable-каналы проекта.
func (n *MetricNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	ev, err := n.reloadEvent(ctx, incidentID, false)
	if err != nil {
		return fmt.Errorf("metric: notify recovery: %w", err)
	}
	_, err = n.dispatch(ctx, ev, channelIDs)
	return err
}

func (n *MetricNotifier) reloadEvent(ctx context.Context, incidentID int64, opened bool) (MetricEvent, error) {
	in, ok, err := n.Incidents.GetByID(ctx, incidentID)
	if err != nil {
		return MetricEvent{}, fmt.Errorf("load incident: %w", err)
	}
	if !ok {
		return MetricEvent{}, fmt.Errorf("incident %d not found", incidentID)
	}
	rule, ok, err := n.Rules.Get(ctx, in.RuleID)
	if err != nil {
		return MetricEvent{}, fmt.Errorf("load rule: %w", err)
	}
	if !ok {
		return MetricEvent{}, fmt.Errorf("rule %d not found", in.RuleID)
	}
	return MetricEvent{
		ProjectID:   in.ProjectID,
		RuleID:      rule.ID,
		MetricName:  rule.MetricName,
		Aggregation: rule.Aggregation,
		Comparator:  rule.Comparator,
		Threshold:   rule.Threshold,
		Current:     in.CurrentValue,
		Peak:        in.PeakValue,
		Environment: rule.Environment,
		LabelKey:    rule.LabelKey,
		LabelValue:  rule.LabelValue,
		Opened:      opened,
	}, nil
}

// channelIDs nil/пусто — все deliverable-каналы; иначе фильтр по членству после
// email-гейта. Возвращает ID реально поставленных каналов — логирует вызывающий.
func (n *MetricNotifier) dispatch(ctx context.Context, ev MetricEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("metric: notify: project channels: %w", err)
	}
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)
	url := fmt.Sprintf("%s/projects/%d/metrics/alerts", n.BaseURL, ev.ProjectID)
	subject := metricSubject(ctx, ev)
	body := metricBody(ctx, ev, url)

	dchans := make([]escalation.DispatchChannel, 0, len(channels))
	for _, ch := range channels {
		dchans = append(dchans, escalation.DispatchChannel{
			ID: ch.ID, Kind: ch.Kind, Target: ch.Target,
			IsEmail:       ch.Kind == alert.ChannelEmail,
			Deliverable:   ch.Deliverable(),
			AllowsDetails: n.Details.AllowsDetails(ch),
		})
	}

	kind := notify.KindMetricAlertResolved
	if ev.Opened {
		kind = notify.KindMetricAlertOpen
	}

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "metric"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: kind, Subject: subject, Body: body,
			URL: url,
			Extra: map[string]any{
				"metric":        ev.MetricName,
				"aggregation":   ev.Aggregation,
				"comparator":    ev.Comparator,
				"threshold":     ev.Threshold,
				"current_value": ev.Current,
				"peak_value":    ev.Peak,
			},
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

func metricSubject(ctx context.Context, ev MetricEvent) string {
	state := i18n.T(ctx, "notify.metric.state.resolved")
	if ev.Opened {
		state = i18n.T(ctx, "notify.metric.state.firing")
	}
	return i18n.Tf(ctx, "notify.metric.subject",
		"metric", ev.MetricName, "agg", ev.Aggregation,
		"cmp", cmpSymbol(ev.Comparator), "threshold", formatNum(ev.Threshold), "state", state)
}

func metricBody(ctx context.Context, ev MetricEvent, url string) string {
	scope := ev.Environment
	if scope == "" {
		scope = i18n.T(ctx, "notify.metric.scope.all_env")
	}
	if ev.LabelKey != "" {
		scope += fmt.Sprintf(", %s=%s", ev.LabelKey, ev.LabelValue)
	}
	key := "notify.metric.body.close"
	if ev.Opened {
		key = "notify.metric.body.open"
	}
	return i18n.Tf(ctx, key,
		"metric", ev.MetricName, "agg", ev.Aggregation,
		"cmp", cmpSymbol(ev.Comparator), "threshold", formatNum(ev.Threshold),
		"current", formatNum(ev.Current), "peak", formatNum(ev.Peak),
		"scope", scope, "url", url)
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
