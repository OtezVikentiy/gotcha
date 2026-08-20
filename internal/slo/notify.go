package slo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// SLOBurnNotifier — алерты об открытии/закрытии инцидента сжигания бюджета SLO
// поверх того же notify.Outbox и тех же каналов проекта, что и остальные
// нотифаеры (trace.RegressionNotifier, uptime.OutboxNotifier): формат payload
// намеренно совпадает с ними — обязательные channel_kind/target читает
// notify.Worker, доставляют те же Sender'ы. Реализует Notifier.
type SLOBurnNotifier struct {
	Alerts *alert.Service // каналы проекта: Alerts.Channels(projectID)
	Outbox *notify.Outbox

	// BaseURL — префикс ссылки на экран деталей SLO в уведомлении:
	// {BaseURL}/projects/{project_id}/slos/{slo_id}.
	BaseURL string

	// EmailEnabled — см. alert.Evaluator.EmailEnabled: пока false, email-каналы
	// пропускаются (с warn-логом), чтобы не ставить в очередь задачи, которые
	// notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. alert.DetailPolicy). Нулевое значение не доверяет никому.
	Details alert.DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык уведомления выбирает оператор.
	Locale i18n.Locale

	// Store — источник перезагрузки SLO+инцидента по ID (B4, T6): планировщик
	// эскалации (T8) хранит только incidentID, у NotifyStep/NotifyRecovery нет
	// готового SLOEvent на входе, как у Notify.
	Store *Store

	// Pool — та же PG, что под Store/Alerts/Outbox: пишет лог эскалации
	// incident_escalations (B4, T6, миграция 0077) после каждого успешного
	// Enqueue в NotifyStep.
	Pool *pgxpool.Pool
}

// Notify ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Интерфейс Notifier не возвращает ошибку (уведомление не должно ронять переход
// инцидента в оценщике): все ошибки логируются, постановка по остальным каналам
// продолжается. Проект без включённых каналов — не ошибка: задач просто не будет.
func (n *SLOBurnNotifier) Notify(ctx context.Context, ev SLOEvent) {
	// dispatch логирует каждую ошибку (channels/enqueue) сама — здесь её
	// достаточно отбросить, Notifier контрактом не возвращает ошибку.
	_ = n.dispatch(ctx, ev, nil, nil)
}

// NotifyStep — эскалационное уведомление открытого инцидента сжигания
// бюджета (B4, T6): повтор OPEN-текста в ЗАДАННЫЕ channelIDs, с логом
// incident_escalations после каждого успешного Enqueue. SLO+инцидент
// грузятся заново по ID — планировщик эскалации (T8) хранит только
// incidentID. channelIDs nil/пусто — все deliverable-каналы проекта (как у
// Notify).
func (n *SLOBurnNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) error {
	ev, err := n.reloadEvent(ctx, incidentID, true)
	if err != nil {
		return fmt.Errorf("slo: notify step: %w", err)
	}
	return n.dispatch(ctx, ev, channelIDs, func(channelID int64) error {
		return escalation.LogStep(ctx, n.Pool, "slo", incidentID, channelID, step)
	})
}

// NotifyRecovery — CLOSE-уведомление инцидента сжигания бюджета (B4, T6) в
// ЗАДАННЫЕ channelIDs, БЕЗ лога incident_escalations (recovery не эскалирует
// — гасит). SLO+инцидент грузятся заново по ID, как в NotifyStep. channelIDs
// nil/пусто — все deliverable-каналы проекта.
func (n *SLOBurnNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	ev, err := n.reloadEvent(ctx, incidentID, false)
	if err != nil {
		return fmt.Errorf("slo: notify recovery: %w", err)
	}
	return n.dispatch(ctx, ev, channelIDs, nil)
}

// reloadEvent перегружает инцидент+SLO по ID и собирает из них SLOEvent
// (B4, T6). Attainment не хранится в slo_incidents — восстанавливается из
// budget_remaining инверсией формулы бюджета (см. budget.go:
// remaining = 1 - (1-attainment)/(1-target)), budget_remaining=nil (бюджет
// не считался на момент открытия) трактуется как «бюджет полностью
// израсходован» (remaining=0) — консервативная нижняя оценка для повторного
// уведомления, не участвующая в решениях оценщика.
func (n *SLOBurnNotifier) reloadEvent(ctx context.Context, incidentID int64, opened bool) (SLOEvent, error) {
	in, ok, err := n.Store.GetIncidentByID(ctx, incidentID)
	if err != nil {
		return SLOEvent{}, fmt.Errorf("load incident: %w", err)
	}
	if !ok {
		return SLOEvent{}, fmt.Errorf("incident %d not found", incidentID)
	}
	s, ok, err := n.Store.Get(ctx, in.ProjectID, in.SLOID)
	if err != nil {
		return SLOEvent{}, fmt.Errorf("load slo: %w", err)
	}
	if !ok {
		return SLOEvent{}, fmt.Errorf("slo %d not found", in.SLOID)
	}
	remaining := 0.0
	if in.BudgetRemaining != nil {
		remaining = *in.BudgetRemaining
	}
	attainment := 1 - (1-remaining)*(1-s.Target)
	return SLOEvent{
		SLO:             s,
		Incident:        in,
		Opened:          opened,
		Attainment:      attainment,
		BudgetRemaining: remaining,
		BurnRate:        in.BurnRate,
	}, nil
}

// dispatch — постановка одной готовой задачи в Outbox. channelIDs (B4, T6) —
// набор каналов, в которые слать: nil/пусто — все deliverable-каналы проекта
// (старое поведение Notify), непустой — фильтр по членству ПОСЛЕ Deliverable/
// email-гейта (эскалация в конкретную ступень лесенки). onEnqueued — если
// задан, дёргается после каждого успешного Enqueue с ID канала (NotifyStep
// пишет им лог incident_escalations); nil — без лога, как раньше.
func (n *SLOBurnNotifier) dispatch(ctx context.Context, ev SLOEvent, channelIDs []int64, onEnqueued func(channelID int64) error) error {
	channels, err := n.Alerts.Channels(ctx, ev.SLO.ProjectID)
	if err != nil {
		slog.Error("slo: burn notify: project channels", "project_id", ev.SLO.ProjectID, "error", err)
		return fmt.Errorf("slo: burn notify: project channels: %w", err)
	}
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)

	url := fmt.Sprintf("%s/projects/%d/slos/%d", n.BaseURL, ev.SLO.ProjectID, ev.SLO.ID)
	subject := sloSubject(ctx, ev)
	body := sloBody(ctx, ev, url)

	// Два вида вместо одного: на обезличенном (трансграничном) пути поле "opened"
	// вырезается, поэтому открытие и закрытие инцидента различает только сам kind
	// (иначе получатель не отличит тревогу от отбоя). Оба зарегистрированы в
	// notify.redactedKindKeys — иначе наружу ушёл бы сырой enum.
	kind := "slo_burn_open"
	if !ev.Opened {
		kind = "slo_burn_close"
	}

	var errs error
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if len(channelIDs) > 0 && !escalation.ContainsID(channelIDs, ch.ID) {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("slo: burn email channel skipped, SMTP not configured",
				"project_id", ev.SLO.ProjectID, "channel_id", ch.ID)
			continue
		}

		// Ловушка имён: адрес канала (webhook URL / chat_id) кладём под "target"
		// — его читает notify.Worker; имя SLO — под "target_name".
		payload := map[string]any{
			"kind":             kind,
			"project_id":       ev.SLO.ProjectID,
			"target_name":      ev.SLO.Name,
			"sli_kind":         string(ev.SLO.Kind),
			"opened":           ev.Opened,
			"attainment":       ev.Attainment,
			"budget_remaining": ev.BudgetRemaining,
			"burn_rate":        ev.BurnRate,
			"url":              url,
			"subject":          subject,
			"body":             body,
			"channel_kind":     ch.Kind,
			"target":           ch.Target,
			// Секрета в payload нет намеренно: notify.Worker достаёт секрет по
			// channel_id в момент отправки (см. notify.SecretResolver).
		}
		// Гейт трансграничной передачи: получателю вне контура оператора уходит
		// обезличенный payload (см. notify.RedactExternalPayload).
		if !n.Details.AllowsDetails(ch) {
			payload = notify.RedactExternalPayload(ctx, payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("slo: burn notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("slo: burn notify: enqueue channel %d: %w", ch.ID, err))
			continue
		}
		if onEnqueued != nil {
			if err := onEnqueued(ch.ID); err != nil {
				errs = errors.Join(errs, err)
			}
		}
	}
	return errs
}

// sloSubject строит тему уведомления по виду перехода (открытие/закрытие) из
// каталога i18n — по локали, положенной в ctx (язык внешнего канала задаёт
// GOTCHA_LOCALE, см. SLOBurnNotifier.Locale).
func sloSubject(ctx context.Context, ev SLOEvent) string {
	if ev.Opened {
		return i18n.Tf(ctx, "notify.slo.open.subject",
			"name", ev.SLO.Name, "burn", formatBurn(ev.BurnRate))
	}
	return i18n.Tf(ctx, "notify.slo.close.subject", "name", ev.SLO.Name)
}

// sloBody строит человекочитаемый текст уведомления: имя SLO, достижение,
// остаток бюджета, скорость сжигания — плюс ссылка на экран деталей. Каталог и
// локаль — как у sloSubject.
func sloBody(ctx context.Context, ev SLOEvent, url string) string {
	att := formatPercent(ev.Attainment)
	budget := formatPercent(ev.BudgetRemaining)
	if ev.Opened {
		return i18n.Tf(ctx, "notify.slo.open.body",
			"name", ev.SLO.Name, "attainment", att, "budget", budget,
			"burn", formatBurn(ev.BurnRate), "url", url)
	}
	return i18n.Tf(ctx, "notify.slo.close.body",
		"name", ev.SLO.Name, "attainment", att, "budget", budget, "url", url)
}

// formatPercent отображает долю ∈ [0,1] одним знаком после запятой в процентах:
// 0.98 → "98.0", 0.3 → "30.0". Остаток бюджета может быть отрицательным
// (перерасход) — знак сохраняется.
func formatPercent(ratio float64) string {
	return fmt.Sprintf("%.1f", ratio*100)
}

// formatBurn отображает множитель burn rate одним знаком после запятой: 20 →
// "20.0", 14.4 → "14.4".
func formatBurn(rate float64) string {
	return fmt.Sprintf("%.1f", rate)
}
