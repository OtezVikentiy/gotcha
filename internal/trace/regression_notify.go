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
// намеренно совпадает с ними — обязательные channel_kind/target читает
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

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. alert.DetailPolicy). Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык уведомления выбирает оператор (№133–136).
	Locale i18n.Locale

	// Regressions — источник перезагрузки регрессии по ID (B4, T6):
	// планировщик эскалации (T8) хранит только incidentID, у NotifyStep/
	// NotifyRecovery нет готового RegressionEvent на входе, как у Notify.
	Regressions *RegressionService

	// Pool — та же PG, что под Regressions/Alerts/Outbox: пишет лог эскалации
	// incident_escalations (B4, T6, миграция 0077) после каждого успешного
	// Enqueue в NotifyStep.
	Pool *pgxpool.Pool

	// Projects — источник имени проекта для темы/тела/webhook-payload
	// уведомления (W3-E). nil-совместим (escalation.ProjectNamer) — тогда
	// уведомления идут без имени проекта, как до этой правки.
	Projects escalation.ProjectNamer
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Ошибка Enqueue по одному каналу не прерывает постановку остальных: все такие
// ошибки логируются и собираются через errors.Join (как в uptime.OutboxNotifier).
// Проект без включённых каналов — не ошибка: задач просто не будет.
func (n *RegressionNotifier) Notify(ctx context.Context, ev RegressionEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// NotifyStep — эскалационное уведомление открытой регрессии (B4, T6): повтор
// OPEN-текста в ЗАДАННЫЕ channelIDs. Возвращает каналы, в которые РЕАЛЬНО
// поставлена задача (deliverable-подмножество channelIDs, прошедшее фильтры
// dispatch) — лог incident_escalations пишет ОРКЕСТРАЦИЯ (escalation.
// SendStepIfDue), не сам нотифаер (реролл B4, T7-fix): лог внутри NotifyStep
// работал только с реальным нотифаером и молчал с мок-нотифаерами тестов, из-
// за чего RecoveryChannels не находил ничего и recovery немел. Регрессия
// грузится заново по ID — планировщик эскалации (T8) хранит только
// incidentID. channelIDs nil/пусто — все deliverable-каналы проекта (как у
// Notify).
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

// NotifyRecovery — CLOSE-уведомление регрессии (B4, T6) в ЗАДАННЫЕ
// channelIDs (recovery не эскалирует — не логируется вообще). Регрессия
// грузится заново по ID, как в NotifyStep. channelIDs nil/пусто — все
// deliverable-каналы проекта.
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

// regressionOpenEvent / regressionCloseEvent собирают RegressionEvent из
// перезагруженной по ID строки perf_regressions (B4, T6): PctIncrease
// пересчитывается из baseline/current (не хранится в таблице), duration —
// от StartedAt до ResolvedAt (уже закрыта к моменту вызова) либо now (ещё не
// закрыта — recovery позвал раньше Resolve).
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

// dispatch — сборка списка каналов проекта и передача готового уведомления в
// общий контур доставки (escalation.Dispatch, W3-E): гейт доставляемости,
// фильтр channelIDs, email-fallback, имя проекта, редакция ПДн. channelIDs
// (B4, T6) — набор каналов, в которые слать: nil/пусто — все
// deliverable-каналы проекта (старое поведение Notify), непустой — фильтр по
// членству ПОСЛЕ Deliverable/email-гейта (эскалация в конкретную ступень
// лесенки). Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена —
// логировать их в incident_escalations или нет, решает вызывающий (эволюатор
// через escalation.SendStepIfDue), не dispatch.
func (n *RegressionNotifier) dispatch(ctx context.Context, ev RegressionEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("trace: regression notify: project channels: %w", err)
	}
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
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
			// Ловушка имён: адрес канала (webhook URL / chat_id) кладём под
			// "target" (собирает сам Dispatch) — его читает notify.Worker;
			// имя цели регрессии — под "target_name".
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

// regressionSubject строит тему уведомления по виду события из каталога i18n
// — по локали, положенной в ctx (№133–136: язык внешнего канала задаёт
// GOTCHA_LOCALE, см. RegressionNotifier.Locale). Тот же ctx питает
// humanize.MetricValue — единую точку форматирования значений метрик.
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

// regressionBody строит человекочитаемый текст уведомления: цель, метрика,
// база/текущее — плюс ссылка на список регрессий. Каталог и локаль — как у
// regressionSubject.
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

// formatPct отображает долю (current-base)/base целым числом процентов:
// 0.5 → "50", 1.5 → "150".
func formatPct(ratio float64) string {
	return fmt.Sprintf("%.0f", ratio*100)
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
