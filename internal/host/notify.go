package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

const (
	hostAlertOpenKind     = "host_alert_open"
	hostAlertResolvedKind = "host_alert_resolved"
	hostRetiredKind       = "host_retired"
)

type depCounter interface {
	DeclaredChildrenCount(ctx context.Context, kind string, nodeID int64) (int, error)
}

type HostNotifier struct {
	Alerts       *alert.Service
	Outbox       *notify.Outbox
	BaseURL      string
	EmailEnabled bool

	// Нулевое значение не раскрывает детали никому.
	Details alert.DetailPolicy

	// Локаль инстанса (GOTCHA_LOCALE), не запроса — внешний канал не знает языка получателя.
	Locale i18n.Locale

	// Источники повторной загрузки инцидента по ID — эскалация хранит только incidentID.
	Incidents *IncidentService
	Hosts     *Store
	Settings  *SettingsService

	// nil — каскад не резолвится, порог в уведомлении берётся из Settings как есть
	// (тестовые и Retirer-инстансы, которым эскалация не звонит NotifyStep).
	Overrides *HostOverrideService
	Groups    *GroupThresholdService

	Pool *pgxpool.Pool

	// nil — строки нет; Retirer оставляет пустым нарочно, шлёт только retire/close.
	DepCounts depCounter

	// nil-совместим — тогда уведомления идут без имени проекта.
	Projects escalation.ProjectNamer
}

// Ошибка возврата всплывает вызывающему — по ней Evaluator решает, ставить ли notified_open.
func (n *HostNotifier) HostIncidentOpened(ctx context.Context, in Incident, h Host, s Settings) error {
	threshold, hasThreshold := thresholdFor(in.Kind, s)
	return n.send(ctx, in, h, true, threshold, hasThreshold)
}

// Порог не передаётся: Settings могли смениться между открытием и закрытием —
// тело показывает только фактическое значение на момент закрытия.
func (n *HostNotifier) HostIncidentResolved(ctx context.Context, in Incident, h Host) error {
	return n.send(ctx, in, h, false, 0, false)
}

// Отдельный вид, а не host_alert_resolved: «вернулось в норму» было бы неверно для снятого с наблюдения.
// Ссылка — на список хостов: карточка этого хоста тут же исчезнет.
func (n *HostNotifier) HostRetired(ctx context.Context, h Host, open []Incident) error {
	ctx = i18n.WithLocale(ctx, n.Locale)
	link := n.listLink(h.ProjectID)
	kinds := kindLabels(ctx, open)
	subject := i18n.Tf(ctx, "notify.host_retired.subject", "host", h.Name)
	body := i18n.Tf(ctx, "notify.host_retired.body",
		"host", h.Name, "kinds", kinds, "url", link)
	// Здесь сырые kind'ы, не подпись: у прочих уведомлений «host_kind» несёт enum, а
	// локализованная строка в соседнем поле дала бы webhook два разных типа под похожими именами.
	rawKinds := make([]string, 0, len(open))
	for _, in := range open {
		rawKinds = append(rawKinds, in.Kind)
	}
	_, err := n.dispatch(ctx, h.ProjectID, hostRetiredKind, subject, body, link, map[string]any{
		"host_id":    h.ID,
		"host_name":  h.Name,
		"host_kinds": rawKinds,
	}, nil)
	return err
}

// Без кавычек-ёлочек вокруг каждого вида: пунктуация списка — дело шаблона локали, не Go.
func kindLabels(ctx context.Context, open []Incident) string {
	labels := make([]string, 0, len(open))
	for _, in := range open {
		labels = append(labels, i18n.T(ctx, "hosts.kind."+in.Kind))
	}
	return strings.Join(labels, ", ")
}

// ok=false для незнакомого kind — не должно случаться, но явный сигнал честнее тихого 0.
func thresholdFor(kind string, s Settings) (float64, bool) {
	switch kind {
	case "disk":
		return s.DiskThreshold, true
	case "memory":
		return s.MemoryThreshold, true
	case "load":
		return s.LoadThreshold, true
	case "silent":
		return s.SilentAfter.Seconds(), true
	default:
		return 0, false
	}
}

func (n *HostNotifier) send(ctx context.Context, in Incident, h Host, opened bool, threshold float64, hasThreshold bool) error {
	// Тексты на языке инстанса, не запроса: у внешнего получателя своей локали нет.
	ctx = i18n.WithLocale(ctx, n.Locale)

	kind := hostAlertOpenKind
	if !opened {
		kind = hostAlertResolvedKind
	}
	link := n.cardLink(in.ProjectID, h.Name)
	extra := map[string]any{
		"host_id":       h.ID,
		"host_name":     h.Name,
		"host_kind":     in.Kind,
		"current_value": in.CurrentValue,
		"peak_value":    in.PeakValue,
	}
	if hasThreshold {
		extra["threshold"] = threshold
	}
	if in.Detail != "" {
		extra["detail"] = in.Detail
	}
	_, err := n.dispatch(ctx, in.ProjectID, kind,
		hostSubject(ctx, in, h, opened),
		hostBody(ctx, in, h, opened, threshold, hasThreshold, link, n.depsLine(ctx, in, h)),
		link, extra, nil)
	return err
}

// Пусто при N=0, ошибке или отсутствии счётчика — не должно зависеть от deps-подсистемы.
func (n *HostNotifier) depsLine(ctx context.Context, in Incident, h Host) string {
	if n.DepCounts == nil || in.Kind != "silent" {
		return ""
	}
	cnt, err := n.DepCounts.DeclaredChildrenCount(ctx, "host", h.ID)
	if err != nil {
		slog.Warn("host: notify: declared children count failed", "host_id", h.ID, "error", err)
		return ""
	}
	if cnt == 0 {
		return ""
	}
	return i18n.Tf(ctx, "notify.host.deps_affected", "count", strconv.Itoa(cnt))
}

// Тот же каскад, что у Evaluator (ThresholdResolver) — иначе повтор уведомления при
// эскалации показал бы порог проекта хосту, у которого он пришпилен оверрайдом.
func (n *HostNotifier) effectiveSettings(ctx context.Context, projectID int64, h Host) (Settings, error) {
	proj, exists, err := n.Settings.GetWithExists(ctx, projectID)
	if err != nil {
		return Settings{}, err
	}
	resolver := ThresholdResolver{Project: proj, ProjectExists: exists}
	if n.Overrides != nil {
		ov, err := n.Overrides.Get(ctx, h.ID)
		if err != nil {
			return Settings{}, err
		}
		resolver.Overrides = map[int64]ThresholdOverride{h.ID: ov}
	}
	if n.Groups != nil {
		groups, err := n.Groups.List(ctx, projectID)
		if err != nil {
			return Settings{}, err
		}
		resolver.Groups = groups
	}
	return resolver.Effective(h).Settings, nil
}

// Лог incident_escalations пишет оркестрация (SendStepIfDue), не этот метод.
// channelIDs nil/пусто — все deliverable-каналы проекта.
func (n *HostNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	in, ok, err := n.Incidents.GetByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("host: notify step: load incident: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("host: notify step: incident %d not found", incidentID)
	}
	hosts, err := n.Hosts.ListByIDs(ctx, []int64{in.HostID})
	if err != nil {
		return nil, fmt.Errorf("host: notify step: load host: %w", err)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("host: notify step: host %d not found", in.HostID)
	}
	h := hosts[0]
	s, err := n.effectiveSettings(ctx, in.ProjectID, h)
	if err != nil {
		return nil, fmt.Errorf("host: notify step: load settings: %w", err)
	}

	ctx = i18n.WithLocale(ctx, n.Locale)
	threshold, hasThreshold := thresholdFor(in.Kind, s)
	link := n.cardLink(in.ProjectID, h.Name)
	extra := map[string]any{
		"host_id":       h.ID,
		"host_name":     h.Name,
		"host_kind":     in.Kind,
		"current_value": in.CurrentValue,
		"peak_value":    in.PeakValue,
	}
	if hasThreshold {
		extra["threshold"] = threshold
	}
	if in.Detail != "" {
		extra["detail"] = in.Detail
	}
	return n.dispatch(ctx, in.ProjectID, hostAlertOpenKind,
		hostSubject(ctx, in, h, true),
		hostBody(ctx, in, h, true, threshold, hasThreshold, link, n.depsLine(ctx, in, h)),
		link, extra, channelIDs)
}

// В отличие от NotifyStep, recovery не логируется нигде — ни здесь, ни в оркестрации.
// channelIDs nil/пусто — все deliverable-каналы проекта.
func (n *HostNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	in, ok, err := n.Incidents.GetByID(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("host: notify recovery: load incident: %w", err)
	}
	if !ok {
		return fmt.Errorf("host: notify recovery: incident %d not found", incidentID)
	}
	hosts, err := n.Hosts.ListByIDs(ctx, []int64{in.HostID})
	if err != nil {
		return fmt.Errorf("host: notify recovery: load host: %w", err)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("host: notify recovery: host %d not found", in.HostID)
	}
	h := hosts[0]

	ctx = i18n.WithLocale(ctx, n.Locale)
	link := n.cardLink(in.ProjectID, h.Name)
	extra := map[string]any{
		"host_id":       h.ID,
		"host_name":     h.Name,
		"host_kind":     in.Kind,
		"current_value": in.CurrentValue,
		"peak_value":    in.PeakValue,
	}
	if in.Detail != "" {
		extra["detail"] = in.Detail
	}
	_, err = n.dispatch(ctx, in.ProjectID, hostAlertResolvedKind,
		hostSubject(ctx, in, h, false),
		hostBody(ctx, in, h, false, 0, false, link, ""),
		link, extra, channelIDs)
	return err
}

// Карточка адресуется именем хоста (id-адресации нет) — listLink нужен, если имя не показать.
func (n *HostNotifier) cardLink(projectID int64, name string) string {
	return fmt.Sprintf("%s/projects/%d/hosts/%s", n.BaseURL, projectID, url.PathEscape(name))
}

func (n *HostNotifier) listLink(projectID int64) string {
	return fmt.Sprintf("%s/projects/%d/hosts", n.BaseURL, projectID)
}

// Возвращает каналы, куда реально поставлено — логировать их решает вызывающая оркестрация.
// channelIDs непустой — фильтр по членству ПОСЛЕ Deliverable/email-гейта, не вместо него.
func (n *HostNotifier) dispatch(ctx context.Context, projectID int64, kind, subject, body, link string, extra map[string]any, channelIDs []int64) ([]int64, error) {
	channels, err := n.Alerts.Channels(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("host: notify: project channels: %w", err)
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
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "host"},
		escalation.DispatchInput{
			ProjectID: projectID, Kind: kind, Subject: subject, Body: body,
			// RedactedURL — listLink, не card: полная ссылка унесла бы имя хоста в Telegram даже без деталей.
			URL: link, RedactedURL: n.listLink(projectID), Extra: extra,
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

// Русские шаблоны ставят {kind} внутри кавычек-ёлочек, не подлежащим фразы: род сказуемого
// не согласовать со всеми видами разом («Тишина — вернулся» звучит криво, «превышен порог «Тишина»» — нет).
func hostSubject(ctx context.Context, in Incident, h Host, opened bool) string {
	key := "notify.host_alert_resolved.subject"
	if opened {
		key = "notify.host_alert_open.subject"
	}
	return i18n.Tf(ctx, key, "host", h.Name, "kind", i18n.T(ctx, "hosts.kind."+in.Kind))
}

func hostBody(ctx context.Context, in Incident, h Host, opened bool, threshold float64, hasThreshold bool, link, depsLine string) string {
	kindLabel := i18n.T(ctx, "hosts.kind."+in.Kind)
	value := ValueLabel(ctx, in.Kind, in.CurrentValue)

	// Пустая строка вместо плейсхолдера — не оставляет висящих меток там, где брать нечего.
	detailLine := ""
	if in.Detail != "" {
		detailLine = i18n.Tf(ctx, "notify.host_alert.detail_line", "detail", in.Detail)
	}

	if opened {
		thresholdLine := ""
		if hasThreshold {
			thresholdLine = i18n.Tf(ctx, "notify.host_alert.threshold_line",
				"threshold", ValueLabel(ctx, in.Kind, threshold))
		}
		return i18n.Tf(ctx, "notify.host_alert_open.body",
			"host", h.Name, "kind", kindLabel, "value", value,
			"threshold_line", thresholdLine, "detail_line", detailLine,
			"deps_line", depsLine, "url", link)
	}
	return i18n.Tf(ctx, "notify.host_alert_resolved.body",
		"host", h.Name, "kind", kindLabel, "value", value,
		"detail_line", detailLine, "url", link)
}
