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

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. alert.DetailPolicy). Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык уведомления выбирает оператор (класс №133–136).
	Locale i18n.Locale

	// Incidents/Rules — источники перезагрузки инцидента по ID (B4, T6):
	// планировщик эскалации (T8) хранит только incidentID, у NotifyStep/
	// NotifyRecovery нет готового MetricEvent на входе, как у Notify.
	Incidents *IncidentService
	Rules     *RuleService

	// Pool — та же PG, что под Incidents/Rules/Alerts/Outbox: пишет лог
	// эскалации incident_escalations (B4, T6, миграция 0077) после каждого
	// успешного Enqueue в NotifyStep.
	Pool *pgxpool.Pool

	// Projects — источник имени проекта для темы/тела/webhook-payload
	// уведомления (W3-E). nil-совместим (escalation.ProjectNamer) — тогда
	// уведомления идут без имени проекта, как до этой правки.
	Projects escalation.ProjectNamer
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает остальные (errors.Join). Проект
// без каналов — не ошибка.
func (n *MetricNotifier) Notify(ctx context.Context, ev MetricEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// NotifyStep — эскалационное уведомление открытого инцидента метрики (B4,
// T6): повтор OPEN-текста в ЗАДАННЫЕ channelIDs. Возвращает каналы, в которые
// РЕАЛЬНО поставлена задача (deliverable-подмножество channelIDs, прошедшее
// фильтры dispatch) — лог incident_escalations пишет ОРКЕСТРАЦИЯ
// (escalation.SendStepIfDue), не сам нотифаер (реролл B4, T7-fix): лог внутри
// NotifyStep работал только с реальным нотифаером и молчал с мок-нотифаерами
// тестов, из-за чего RecoveryChannels не находил ничего и recovery немел.
// Инцидент/правило грузятся заново по ID — планировщик эскалации (T8) хранит
// только incidentID. channelIDs nil/пусто — все deliverable-каналы проекта
// (как у Notify).
func (n *MetricNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	ev, err := n.reloadEvent(ctx, incidentID, true)
	if err != nil {
		return nil, fmt.Errorf("metric: notify step: %w", err)
	}
	return n.dispatch(ctx, ev, channelIDs)
}

// NotifyRecovery — CLOSE-уведомление инцидента метрики (B4, T6) в ЗАДАННЫЕ
// channelIDs (recovery не эскалирует — не логируется вообще). Инцидент/
// правило грузятся заново по ID, как в NotifyStep. channelIDs nil/пусто —
// все deliverable-каналы проекта.
func (n *MetricNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	ev, err := n.reloadEvent(ctx, incidentID, false)
	if err != nil {
		return fmt.Errorf("metric: notify recovery: %w", err)
	}
	_, err = n.dispatch(ctx, ev, channelIDs)
	return err
}

// reloadEvent перегружает инцидент+правило по ID и собирает из них
// MetricEvent — общая часть NotifyStep/NotifyRecovery (B4, T6).
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

// dispatch — сборка списка каналов проекта и передача готового уведомления в
// общий контур доставки (escalation.Dispatch, W3-E): гейт доставляемости,
// фильтр channelIDs, email-fallback, имя проекта, редакция ПДн. channelIDs
// (B4, T6) — набор каналов, в которые слать: nil/пусто — все
// deliverable-каналы проекта (старое поведение Notify), непустой — фильтр по
// членству ПОСЛЕ Deliverable/email-гейта (эскалация в конкретную ступень
// лесенки). Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена —
// логировать их в incident_escalations или нет, решает вызывающий (эволюатор
// через escalation.SendStepIfDue), не dispatch.
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

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "metric"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: metricEventKind(ev), Subject: subject, Body: body,
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

func metricEventKind(ev MetricEvent) string {
	if ev.Opened {
		return "metric_alert_open"
	}
	return "metric_alert_resolved"
}

// metricSubject / metricBody строят тексты из каталога i18n — по локали,
// положенной в ctx (класс №133–136: язык внешнего канала задаёт
// GOTCHA_LOCALE, см. MetricNotifier.Locale).
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
