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

	// Regressions — источник перезагрузки регрессии по ID (B4, T6):
	// планировщик эскалации (T8) хранит только incidentID, у NotifyStep/
	// NotifyRecovery нет готового ProfileRegressionEvent на входе, как у Notify.
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
// Ошибка Enqueue по одному каналу не прерывает остальные (errors.Join).
func (n *RegressionNotifier) Notify(ctx context.Context, ev ProfileRegressionEvent) error {
	_, err := n.dispatch(ctx, ev, nil)
	return err
}

// NotifyStep — эскалационное уведомление открытой регрессии профиля (B4,
// T6): повтор OPEN-текста в ЗАДАННЫЕ channelIDs. Возвращает каналы, в которые
// РЕАЛЬНО поставлена задача (deliverable-подмножество channelIDs, прошедшее
// фильтры dispatch) — лог incident_escalations пишет ОРКЕСТРАЦИЯ (escalation.
// SendStepIfDue), не сам нотифаер (реролл B4, T7-fix): лог внутри NotifyStep
// работал только с реальным нотифаером и молчал с мок-нотифаерами тестов, из-
// за чего RecoveryChannels не находил ничего и recovery немел. Регрессия
// грузится заново по ID — планировщик эскалации (T8) хранит только
// incidentID. channelIDs nil/пусто — все deliverable-каналы проекта (как у
// Notify).
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

// NotifyRecovery — CLOSE-уведомление регрессии профиля (B4, T6) в ЗАДАННЫЕ
// channelIDs (recovery не эскалирует — не логируется вообще). Регрессия
// грузится заново по ID, как в NotifyStep. channelIDs nil/пусто — все
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

// profileRegressionEvent собирает ProfileRegressionEvent из перезагруженной
// по ID строки profile_regressions (B4, T6): PctIncrease пересчитывается из
// baseline/current долей тем же pctIncrease, что использует оценщик
// (reg_evaluator.go), а не хранится в таблице отдельно.
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

// dispatch — сборка списка каналов проекта и передача готового уведомления в
// общий контур доставки (escalation.Dispatch, W3-E): гейт доставляемости,
// фильтр channelIDs, email-fallback, имя проекта, редакция ПДн. channelIDs
// (B4, T6) — набор каналов, в которые слать: nil/пусто — все
// deliverable-каналы проекта (старое поведение Notify), непустой — фильтр по
// членству ПОСЛЕ Deliverable/email-гейта (эскалация в конкретную ступень
// лесенки). Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена —
// логировать их в incident_escalations или нет, решает вызывающий (эволюатор
// через escalation.SendStepIfDue), не dispatch.
func (n *RegressionNotifier) dispatch(ctx context.Context, ev ProfileRegressionEvent, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, ev.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("profile: regression notify: project channels: %w", err)
	}
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
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

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "profile"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: regressionKind(ev), Subject: subject, Body: body,
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
