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

// Kinds оутбокса встроенных инцидентов хоста — калька
// metric_alert_open/metric_alert_resolved (см. internal/notify/redact.go,
// redactedKindKeys).
const (
	hostAlertOpenKind     = "host_alert_open"
	hostAlertResolvedKind = "host_alert_resolved"
	// hostRetiredKind — хост снят с наблюдения по ретенции (см.
	// HostNotifier.HostRetired и Retirer).
	hostRetiredKind = "host_retired"
)

// depCounter — счётчик задекларированных детей узла (D3 Р9,
// depsuppress.Suppressor.DeclaredChildrenCount). Duck-typed, как Notifier у
// Evaluator: пакету host не нужен весь Suppressor, а nil-значение законно —
// уведомления не зависят от D3.
type depCounter interface {
	DeclaredChildrenCount(ctx context.Context, kind string, nodeID int64) (int, error)
}

// HostNotifier рассылает уведомления об открытии/закрытии встроенных
// инцидентов хоста (диск/память/нагрузка/тишина) через общий outbox по
// каналам проекта — калька metric.MetricNotifier. Реализует host.Notifier.
type HostNotifier struct {
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

	// Incidents/Hosts/Settings — источники перезагрузки инцидента по ID (B4,
	// T6): планировщик эскалации (T8) хранит только incidentID, у NotifyStep/
	// NotifyRecovery нет готового Incident/Host/Settings на входе, как у
	// HostIncidentOpened/Resolved.
	Incidents *IncidentService
	Hosts     *Store
	Settings  *SettingsService

	// Pool — та же PG, что под Incidents/Hosts/Alerts/Outbox: пишет лог
	// эскалации incident_escalations (B4, T6, миграция 0077) после каждого
	// успешного Enqueue в NotifyStep.
	Pool *pgxpool.Pool

	// DepCounts — источник числа задекларированных детей хоста для строки
	// «Зависимых узлов: N» в open-уведомлении инцидента недоступности (D3
	// Р9, см. depsLine). nil — строки нет; Retirer-экземпляр в main.go
	// оставляет поле пустым намеренно (он шлёт только retire/close).
	DepCounts depCounter

	// Projects — источник имени проекта для темы/тела/webhook-payload
	// уведомления (W3-E). nil-совместим (escalation.ProjectNamer) — тогда
	// уведомления идут без имени проекта, как до этой правки.
	Projects escalation.ProjectNamer
}

// HostIncidentOpened реализует host.Notifier: инцидент открыт, ставит задачу
// в Outbox на каждый deliverable-канал проекта. Ошибка постановки всплывает
// вызывающему — по ней Evaluator решает, честно ли ставить notified_open.
func (n *HostNotifier) HostIncidentOpened(ctx context.Context, in Incident, h Host, s Settings) error {
	threshold, hasThreshold := thresholdFor(in.Kind, s)
	return n.send(ctx, in, h, true, threshold, hasThreshold)
}

// HostIncidentResolved реализует host.Notifier: инцидент закрыт. Порог
// контрактом интерфейса сюда не передаётся (Settings могли смениться между
// открытием и закрытием инцидента) — сообщение показывает только фактическое
// значение на момент закрытия.
func (n *HostNotifier) HostIncidentResolved(ctx context.Context, in Incident, h Host) error {
	return n.send(ctx, in, h, false, 0, false)
}

// HostRetired реализует RetireNotifier: хост не присылал данные дольше срока
// хранения метрик, его открытые инциденты закрываются и сам он удаляется (см.
// Retirer). Одно сообщение на хост, а не на инцидент: у мёртвого сервера
// открытыми висят все пороги разом (оценщик считает только живые хосты, см.
// Evaluator.Tick), и письмо на каждый было бы четырёхкратным шумом об одном
// событии.
//
// Отдельный вид, а не host_alert_resolved: текст закрытия говорит «порог
// вернулся в норму», что про снимаемый с наблюдения сервер прямо неверно —
// оператор прочитал бы «машина ожила» ровно там, где она окончательно ушла.
//
// Ссылка ведёт на список хостов: карточки этого хоста через мгновение не
// станет, и звать по ней некуда.
func (n *HostNotifier) HostRetired(ctx context.Context, h Host, open []Incident) error {
	ctx = i18n.WithLocale(ctx, n.Locale)
	link := n.listLink(h.ProjectID)
	kinds := kindLabels(ctx, open)
	subject := i18n.Tf(ctx, "notify.host_retired.subject", "host", h.Name)
	body := i18n.Tf(ctx, "notify.host_retired.body",
		"host", h.Name, "kinds", kinds, "url", link)
	// host_kinds — сырые виды закрытых порогов, а не подпись из kinds:
	// «host_kind» у остальных уведомлений хоста несёт enum, и класть в
	// соседнее поле локализованное перечисление значило бы отдать webhook-
	// получателю два разных типа под похожими именами. Подпись для человека
	// уже есть в subject/body.
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

// kindLabels — виды закрываемых инцидентов человекочитаемым перечислением
// («Диск, Тишина»). Без кавычек-ёлочек вокруг каждого: перечисление подставляется
// в шаблон целиком, и пунктуация списка — дело шаблона локали, а не Go.
func kindLabels(ctx context.Context, open []Incident) string {
	labels := make([]string, 0, len(open))
	for _, in := range open {
		labels = append(labels, i18n.T(ctx, "hosts.kind."+in.Kind))
	}
	return strings.Join(labels, ", ")
}

// thresholdFor возвращает порог вида инцидента из Settings проекта.
// ok=false для незнакомого kind (не должно случаться — Kind приходит из
// Kinds, но явная сигнатура честнее, чем молчаливый 0).
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

// send — общая постановка задачи в Outbox для открытия и закрытия: строит
// тексты по локали инстанса и рассылает по всем deliverable-каналам проекта.
func (n *HostNotifier) send(ctx context.Context, in Incident, h Host, opened bool, threshold float64, hasThreshold bool) error {
	// Тексты — на языке инстанса, а не запроса: уведомление читает внешний
	// получатель, у которого нет своей локали.
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

// depsLine — строка «Зависимых узлов: N» для open-уведомления инцидента
// НЕДОСТУПНОСТИ (kind='silent', Р9): N — число задекларированных детей
// одного уровня (нейтральная формулировка MINOR-7). Пусто при N=0, ошибке
// или отсутствии счётчика — уведомление не должно зависеть от D3.
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

// NotifyStep — эскалационное уведомление открытого инцидента хоста (B4, T6):
// повтор OPEN-текста (эскалация — это повтор открывающего алерта, не новый
// вид события) в ЗАДАННЫЕ channelIDs. Возвращает каналы, в которые РЕАЛЬНО
// поставлена задача (deliverable-подмножество channelIDs, прошедшее фильтры
// dispatch) — лог incident_escalations пишет ОРКЕСТРАЦИЯ (escalation.
// SendStepIfDue), а не сам нотифаер (реролл B4, T7-fix): логирование внутри
// NotifyStep работало только с реальным нотифаером и молчало с мок-
// нотифаерами тестов, из-за чего RecoveryChannels (лог) не находил ничего и
// recovery немо. Инцидент/хост/настройки грузятся заново по ID — планировщик
// эскалации (T8) хранит только incidentID, не сам объект. channelIDs
// nil/пусто — все deliverable-каналы проекта (как у HostIncidentOpened).
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
	s, err := n.Settings.Get(ctx, in.ProjectID)
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

// NotifyRecovery — CLOSE-уведомление инцидента хоста (B4, T6) в ЗАДАННЫЕ
// channelIDs (recovery не эскалирует — не логируется вообще, ни здесь, ни в
// оркестрации). Инцидент/хост грузятся заново по ID, как в NotifyStep.
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

// cardLink / listLink — адреса карточки хоста и списка хостов проекта.
// Карточка адресуется ИМЕНЕМ машины (id-адресации у хоста нет), поэтому
// вторая нужна везде, где имя показывать нельзя или показывать уже нечего.
func (n *HostNotifier) cardLink(projectID int64, name string) string {
	return fmt.Sprintf("%s/projects/%d/hosts/%s", n.BaseURL, projectID, url.PathEscape(name))
}

func (n *HostNotifier) listLink(projectID int64) string {
	return fmt.Sprintf("%s/projects/%d/hosts", n.BaseURL, projectID)
}

// dispatch — сборка списка каналов проекта и передача готового уведомления в
// общий контур доставки (escalation.Dispatch, W3-E): гейт доставляемости,
// фильтр channelIDs, email-fallback, имя проекта, редакция ПДн — всё это
// раньше было переписано здесь же (седьмая копия из семи, см. отчёт W3-E) и
// уже успело разойтись с остальными шестью (ContainsID, адресность
// recovery). Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена —
// логировать их в incident_escalations или нет, решает вызывающая
// оркестрация (escalation.SendStepIfDue), не dispatch: тот же enqueued-
// список нужен и NotifyStep (лог), и никому больше.
//
// extra — поля payload сверх маршрутного минимума (имя хоста, значения,
// порог): доменная специфика host, контур сам их не строит, только
// подмешивает и вырезает гейтом трансграничной передачи при обезличивании.
//
// channelIDs (B4, T6) — набор каналов, в которые слать: nil/пусто — все
// deliverable-каналы проекта (старое поведение open/close/retired, см. send/
// HostRetired), непустой — фильтр по членству ПОСЛЕ Deliverable/email-гейта
// (эскалация в конкретную ступень лесенки, NotifyStep/NotifyRecovery).
//
// listLink как RedactedURL — карточка хоста адресуется именем машины
// (/projects/{id}/hosts/{name}, id-адресации у хоста нет), и полная ссылка
// унесла бы имя в Telegram даже при выключенных деталях; список хостов
// проекта такой детали не несёт.
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
			URL: link, RedactedURL: n.listLink(projectID), Extra: extra,
			ChannelIDs: channelIDs, Channels: dchans,
		})
}

// hostSubject / hostBody строят тексты из каталога i18n — по локали, положенной
// в ctx (класс №133–136: язык внешнего канала задаёт GOTCHA_LOCALE, см.
// HostNotifier.Locale). Вид порога — динамическая группа "hosts.kind." от
// Kinds (см. internal/guards/i18n_dynamic_test.go).
//
// Русские шаблоны подставляют {kind} ВНУТРЬ кавычек-ёлочек («превышен порог
// «Память»»), а не делают его подлежащим фразы (UX-аудит A1, P1-2). Раньше
// было «{kind} — вернулся в норму», и подстановка давала «Память — вернулся»,
// «Тишина — вернулся»: род сказуемого нельзя согласовать со списком видов,
// у которых он разный. Второй дефект того же текста — «превышена тишина»:
// молчание хоста порогом «превышается» только формально, поэтому оба
// сообщения говорят о ПОРОГЕ, а не о величине. Английские шаблоны
// ("{kind} threshold breached") согласования не требуют и оставлены как есть.
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

	// detailLine/thresholdLine — необязательные вставки: пустая строка не
	// оставляет в теле сообщения ни висящего плейсхолдера, ни пустой строки
	// там, где для disk/memory/load и т.п. взять нечего.
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
