package slo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

type SLOBurnNotifier struct {
	Alerts *alert.Service
	Outbox *notify.Outbox

	// {BaseURL}/projects/{project_id}/slos/{slo_id}.
	BaseURL string

	// пока false, email-каналы пропускаются (с warn-логом) — иначе в очередь
	// попадут задачи, которые notify.Worker всё равно не доставит.
	EmailEnabled bool

	// нулевое значение политики не раскрывает детали никому.
	Details alert.DetailPolicy

	// язык из GOTCHA_LOCALE: внешний канал не знает локали получателя, её выбирает оператор.
	Locale i18n.Locale

	// нужен для перезагрузки SLO+инцидента по ID: NotifyStep/NotifyRecovery
	// получают только incidentID, без готового события.
	Store *Store

	Pool *pgxpool.Pool

	// nil-совместим: без него уведомления идут без имени проекта.
	Projects escalation.ProjectNamer
}

// интерфейс Notifier не возвращает ошибку — уведомление не должно ронять переход
// инцидента в оценщике; все ошибки только логируются.
func (n *SLOBurnNotifier) Notify(ctx context.Context, ev SLOEvent) {
	_, _ = n.dispatch(ctx, ev, nil)
}

// лог incident_escalations пишет вызывающая оркестрация, не сам NotifyStep.
// channelIDs nil/пусто — все deliverable-каналы проекта.
func (n *SLOBurnNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	ev, err := n.reloadEvent(ctx, incidentID, true)
	if err != nil {
		return nil, fmt.Errorf("slo: notify step: %w", err)
	}
	return n.dispatch(ctx, ev, channelIDs)
}

// recovery не эскалирует — не логируется вообще; channelIDs nil/пусто — все
// deliverable-каналы проекта.
func (n *SLOBurnNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	ev, err := n.reloadEvent(ctx, incidentID, false)
	if err != nil {
		return fmt.Errorf("slo: notify recovery: %w", err)
	}
	_, err = n.dispatch(ctx, ev, channelIDs)
	return err
}

// Attainment не хранится — восстанавливается из budget_remaining инверсией формулы;
// budget_remaining=nil трактуется как remaining=0 (консервативная нижняя оценка).
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

// channelIDs nil/пусто — все deliverable-каналы проекта; непустой — фильтр по членству после гейта.
// Логировать возвращённые каналы в incident_escalations или нет — решает вызывающий, не dispatch.
func (n *SLOBurnNotifier) dispatch(ctx context.Context, ev SLOEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.SLO.ProjectID)
	if err != nil {
		slog.Error("slo: burn notify: project channels", "project_id", ev.SLO.ProjectID, "error", err)
		return nil, fmt.Errorf("slo: burn notify: project channels: %w", err)
	}
	// язык инстанса, а не запроса: читает внешний получатель без своей локали.
	ctx = i18n.WithLocale(ctx, n.Locale)

	url := fmt.Sprintf("%s/projects/%d/slos/%d", n.BaseURL, ev.SLO.ProjectID, ev.SLO.ID)
	subject := sloSubject(ctx, ev)
	body := sloBody(ctx, ev, url)

	// два kind вместо одного: поле "opened" уходит через Extra и вырезается на
	// обезличенном пути — без разных kind получатель не отличил бы тревогу от отбоя.
	kind := notify.KindSLOBurnOpen
	if !ev.Opened {
		kind = notify.KindSLOBurnClose
	}

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
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "slo"},
		escalation.DispatchInput{
			ProjectID: ev.SLO.ProjectID, Kind: kind, Subject: subject, Body: body,
			URL: url,
			// адрес канала кладёт сам Dispatch под "target" (читает notify.Worker) —
			// имя SLO здесь идёт под "target_name", не перепутать.
			Extra: map[string]any{
				"target_name":      ev.SLO.Name,
				"sli_kind":         string(ev.SLO.Kind),
				"opened":           ev.Opened,
				"attainment":       ev.Attainment,
				"budget_remaining": ev.BudgetRemaining,
				"burn_rate":        ev.BurnRate,
			},
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

func sloSubject(ctx context.Context, ev SLOEvent) string {
	if ev.Opened {
		return i18n.Tf(ctx, "notify.slo.open.subject",
			"name", ev.SLO.Name, "burn", formatBurn(ev.BurnRate))
	}
	return i18n.Tf(ctx, "notify.slo.close.subject", "name", ev.SLO.Name)
}

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

// доля → проценты с одним знаком после запятой (0.98 → "98.0"); остаток
// бюджета может быть отрицательным (перерасход), знак сохраняется.
func formatPercent(ratio float64) string {
	return fmt.Sprintf("%.1f", ratio*100)
}

func formatBurn(rate float64) string {
	return fmt.Sprintf("%.1f", rate)
}
