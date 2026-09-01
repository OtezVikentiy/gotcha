// Package nav holds the navigation model shared by the web layer and
// templates: the shell state carried in the request context, and the
// static information architecture (icon-rail areas + contextual
// sidebar subsections). It has no dependency on web/templates so both
// can import it without creating a cycle.
package nav

import (
	"context"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

// Project is a minimal project reference used by the project switcher.
type Project struct {
	ID   int64
	Slug string
	Name string
	// OrgID — организация проекта. Нужен web-слою, чтобы сужать полный
	// список проектов пользователя (projs) до проектов ТЕКУЩЕЙ организации
	// без второго похода в БД (shellProjects — фильтр в памяти, см.
	// internal/web/shell.go).
	OrgID int64
}

// Org — минимальная ссылка на организацию для селекта в топбаре (задача 4
// nav-ia): переключатель проекта переехал из контекстной колонки в топбар,
// над ним встал селект организации, сужающий список проектов ниже.
type Org struct {
	ID   int64
	Name string
}

// NavItem is a single sidebar subsection entry. LabelKey is an i18n key,
// rendered via i18n.T in templates. Label, when non-empty, is rendered
// directly instead — used by areas (e.g. docs) whose item labels come
// from content (a markdown H1) rather than the i18n catalog.
type NavItem struct {
	LabelKey string
	Label    string
	Href     string
	Active   bool
	// Group — i18n-ключ заголовка группы (nav.group.*), в которую входит
	// пункт. Пустая строка — пункт вне групп. Заголовок рендерится один раз
	// при смене группы; группа, все пункты которой отфильтрованы правами,
	// исчезает вместе с заголовком (иначе оператор без CanManage увидел бы в
	// «Настройках» заголовок «Организация» без содержимого).
	Group string
}

// NavGroup — подразделы одной группы контекстной колонки. Пустой LabelKey
// означает «рендерить без заголовка».
type NavGroup struct {
	LabelKey string
	Items    []NavItem
}

// GroupedSubsections — подразделы текущей области, разложенные по группам
// в порядке их первого появления.
func GroupedSubsections(s Shell) []NavGroup {
	return groupItems(Subsections(s))
}

// groupItems раскладывает items по группам, объединяя подряд идущие пункты
// одной группы (включая пустую — «без группы») в один NavGroup. Порядок
// групп — порядок первого появления в items.
func groupItems(items []NavItem) []NavGroup {
	var groups []NavGroup
	for _, it := range items {
		if n := len(groups); n > 0 && groups[n-1].LabelKey == it.Group {
			groups[n-1].Items = append(groups[n-1].Items, it)
			continue
		}
		groups = append(groups, NavGroup{LabelKey: it.Group, Items: []NavItem{it}})
	}
	return groups
}

// NavArea is a single icon-rail entry.
type NavArea struct {
	ID       string
	IconName string
	LabelKey string
	Href     string
	Active   bool
	// Footer — область принадлежит подвалу рейла (рендерится после
	// распорки, ниже рабочих областей, рядом с аватаром и выходом).
	Footer bool
}

// Shell is the app-shell state carried through the request context: the
// current user, their projects (for the switcher), the current
// project/org, the active rail area, and the current request path (used
// to compute Active on nav items).
type Shell struct {
	UserEmail string
	Projects  []Project
	// Orgs — организации пользователя, для селекта в топбаре (задача 4
	// nav-ia). Проектный переключатель ниже показывает Projects, уже
	// суженные выбранной (OrgID) организацией — см. web/shell.go.
	Orgs      []Org
	ProjectID int64
	OrgID     int64
	Area      string
	Path      string
	// Origin — подраздел, из которого пользователь пришёл на общую страницу
	// (?from= в адресе, значение уже проверено в web-слое). Нужен там, где по
	// одному адресу можно попасть из разных мест: страница эндпойнта общая
	// для «Транзакций» и «Web Vitals», трейс открывается из трёх разделов.
	// Без него подсветка молча уезжала в соседний подраздел и спорила с
	// хлебной крошкой.
	Origin string
	// Locale is the request's resolved i18n locale ("ru"/"en"), used to
	// build the docs area's Subsections (doc page titles come from
	// localized markdown H1s, via internal/docs, not the i18n catalog).
	Locale string
	// CanManage indicates whether the current user is owner/admin of the
	// current org (OrgID). It gates org-management links (Members/Teams/
	// Probes in the org subsections, and the project-settings link in the
	// layout) so plain members never see links to pages that 404 for them.
	CanManage bool
	// CanOperate — является ли текущий пользователь оператором ТЕКУЩЕГО
	// проекта (скоуп OrgID+ProjectID): участник команды с доступом к проекту
	// либо owner/admin организации. В отличие от CanManage — скоуп
	// проектный, не организационный: гейтит пункты мониторинга
	// (metric_alerts, maintenance, status_pages, область alerts), которые
	// требуют requireProjectOperator, а не requireProjectRole.
	CanOperate bool
	// ExportsEnabled — фича выгрузок ошибок сконфигурирована на инстансе
	// (h.Exports != nil в web-слое: каталог выгрузок доступен и создаваем,
	// см. web.go/withShell). Гейтит пункт «Выгрузки» рядом с CanOperate: на
	// инстансе без каталога воркер не стартует, и пункт меню, ведущий на
	// страницу заявок, которые никогда не досчитаются, — введение в
	// заблуждение, а не полезная ссылка (спека E1 §10: «кнопки скрыты»).
	ExportsEnabled bool
	// Back — валидированный same-origin относительный путь «откуда пришёл»
	// (из заголовка Referer, посчитан в web-слое). Пустой, если Referer
	// отсутствует, ведёт на текущую же страницу (перезагрузка) или на чужой
	// origin. Питает хлебную крошку «назад»: страница-деталь возвращает туда,
	// откуда на неё пришли, а не на жёстко зашитого родителя.
	Back string
}

type ctxKey struct{}

// WithShell stores s in ctx.
func WithShell(ctx context.Context, s Shell) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the Shell stored in ctx, or a zero Shell if absent.
func FromContext(ctx context.Context) Shell {
	s, _ := ctx.Value(ctxKey{}).(Shell)
	return s
}

// effectiveProjectID returns s.ProjectID, falling back to the first of
// s.Projects when no current project is set.
func effectiveProjectID(s Shell) int64 {
	if s.ProjectID != 0 {
		return s.ProjectID
	}
	if len(s.Projects) > 0 {
		return s.Projects[0].ID
	}
	return 0
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// railAreas — статический упорядоченный список областей рейла. Три яруса,
// разделённые в вёрстке распорками: работа (issues…uptime), настройка
// (alerts), служебное (settings, docs). Порядок наблюдательных областей —
// по частоте обращения в разборе: сначала то, что система нашла сама,
// потом измерения, потом инфраструктура, потом доступность.
var railAreas = []struct {
	id, icon, labelKey string
	footer             bool
}{
	{"issues", "bug", "nav.issues", false},
	{"performance", "zap", "nav.performance", false},
	{"logs", "file-text", "nav.logs", false},
	{"metrics", "chart", "nav.metrics", false},
	{"hosts", "server", "nav.hosts", false},
	{"uptime", "activity", "nav.uptime", false},
	{"alerts", "bell", "nav.alerts", false},
	{"settings", "settings", "nav.settings", true},
}

// AreaForPath maps a request path to a rail area id, per the information
// architecture. It returns "" when the path does not belong to any area
// (e.g. project settings, setup, or an unrecognized path).
func AreaForPath(path string) string {
	switch {
	case path == "/docs", strings.HasPrefix(path, "/docs/"):
		return "docs"
	case strings.HasPrefix(path, "/issues"):
		return "issues"
	case strings.HasPrefix(path, "/traces/"):
		return "performance"
	case strings.HasPrefix(path, "/perf-issues/"):
		return "issues"
	case strings.HasPrefix(path, "/monitors/"), strings.HasPrefix(path, "/statuspages/"):
		return "uptime"
	case strings.HasPrefix(path, "/orgs/"):
		// /orgs/{id}/settings|teams|probes — управление организацией,
		// переехавшее в задаче 4 в группу «Организация» внутри области
		// «Настройки» (см. Subsections, case "settings"). /orgs/{id}/projects
		// (список проектов организации) и /profile ни в какую область не
		// входят — область "" для них осознанна (задача 5).
		rest := strings.TrimPrefix(path, "/orgs/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 {
			switch parts[1] {
			case "settings", "teams", "probes":
				return "settings"
			}
		}
		return ""
	case path == "/setup", strings.HasPrefix(path, "/setup/"):
		return ""
	}

	if strings.HasPrefix(path, "/projects/") {
		rest := strings.TrimPrefix(path, "/projects/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 {
			if _, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				switch parts[1] {
				case "issues", "exports", "perf-issues", "regressions":
					return "issues"
				case "performance", "web-vitals", "profiles", "profile-regressions", "dependencies", "deployments":
					return "performance"
				case "metrics":
					// /metrics/alerts is a rule (когда сработает), grouped
					// under «Оповещения» with the rest of the alerting
					// configuration; the plain metrics list stays put.
					if len(parts) >= 3 && parts[2] == "alerts" {
						return "alerts"
					}
					return "metrics"
				case "hosts":
					// /hosts/settings configures thresholds — a rule, same
					// as above; the host list itself stays put.
					if len(parts) >= 3 && parts[2] == "settings" {
						return "alerts"
					}
					return "hosts"
				case "logs":
					return "logs"
				case "monitors", "incidents":
					return "uptime"
				case "alerts", "slos", "escalations", "alert-suppression", "maintenance":
					return "alerts"
				case "overview", "incident-feed":
					// Лента инцидентов (D3) переехала на «Обзор» (задача 6
					// nav-ia); старый адрес /incident-feed редиректит туда
					// же (см. web/overview.go), поэтому подсвечивает ту же
					// область, а не исчезает молча.
					return "overview"
				case "statuspages", "recipes", "settings", "setup":
					// Not a rail area (nothing lights up in the rail), but
					// it does own the sidebar: without it the sidebar shows
					// only the project switcher and reads as broken.
					return "settings"
				}
			}
		}
	}
	return ""
}

// BackLabelKey возвращает i18n-ключ подписи для ссылки «назад» по её пути.
// Для страниц вида /projects/{id}/{section} подпись берётся по самому разделу
// (та же таблица, что и в AreaForPath/Subsections), поэтому «← Инциденты» ведут
// именно в инциденты, а не в общую «Аптайм». Корневые detail-адреса и прочее
// опознаются по области. Пустая строка — путь не опознан; вызывающий
// подставляет общий "nav.back".
func BackLabelKey(rawPath string) string {
	path := rawPath
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if strings.HasPrefix(path, "/projects/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/projects/"), "/", 4)
		if len(parts) >= 2 {
			switch parts[1] {
			case "overview", "incident-feed":
				return "nav.overview"
			case "issues":
				return "nav.errors"
			case "exports":
				return "nav.exports"
			case "performance":
				return "nav.transactions"
			case "web-vitals":
				return "nav.webvitals"
			case "profiles", "profile-regressions":
				return "nav.profiles"
			case "perf-issues":
				return "nav.perf_issues"
			case "regressions":
				return "nav.regressions"
			case "dependencies":
				return "nav.dependencies"
			case "deployments":
				return "nav.deployments"
			case "metrics":
				if len(parts) >= 3 && parts[2] == "alerts" {
					return "nav.metric_alerts"
				}
				return "nav.metrics"
			case "recipes":
				return "nav.recipes"
			case "hosts":
				if len(parts) >= 3 && parts[2] == "settings" {
					return "nav.host_thresholds"
				}
				return "nav.hosts"
			case "logs":
				return "nav.logs"
			case "monitors":
				return "nav.monitors"
			case "incidents":
				return "nav.incidents"
			case "maintenance":
				return "nav.maintenance"
			case "statuspages":
				return "nav.status_pages"
			case "alerts":
				if len(parts) >= 3 && parts[2] == "deliveries" {
					return "nav.alert_deliveries"
				}
				return "nav.rules_errors"
			case "slos":
				return "nav.slo"
			case "escalations":
				return "nav.escalations"
			case "alert-suppression":
				return "nav.alert_suppression"
			case "settings", "setup":
				return "nav.project_settings"
			}
		}
	}
	// /orgs/{id}/... — подпись крошки «назад» берётся не по имени области
	// (settings/teams/probes все живут в одной, "settings"), а по имени
	// самого пункта сайдбара, на который страница и есть (Subsections,
	// case "settings", группа "nav.group.org"): settings → «Участники»,
	// teams → «Команды», probes → «Пробы». /projects — «Проекты».
	if strings.HasPrefix(path, "/orgs/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/orgs/"), "/", 3)
		if len(parts) >= 2 {
			switch parts[1] {
			case "settings":
				return "nav.members"
			case "teams":
				return "nav.teams"
			case "probes":
				return "nav.probes"
			case "projects":
				return "nav.projects"
			}
		}
	}
	switch AreaForPath(path) {
	case "overview":
		return "nav.overview"
	case "issues":
		return "nav.issues"
	case "performance":
		return "nav.performance"
	case "metrics":
		return "nav.metrics"
	case "uptime":
		return "nav.monitors"
	case "alerts":
		return "nav.alerts"
	case "settings":
		return "nav.project_settings"
	case "docs":
		return "docs.index.title"
	}
	return ""
}

// Subsections returns the sidebar subsections for s.Area, scoped to the
// effective project/org, with Active set for the item whose Href is the
// longest prefix match of s.Path.
func Subsections(s Shell) []NavItem {
	effID := itoa(effectiveProjectID(s))
	orgID := itoa(s.OrgID)

	var items []NavItem
	switch s.Area {
	case "issues":
		// Находки детекторов (узкие места, регрессии) живут рядом с общим
		// списком ошибок — все три читают один и тот же поток проблем
		// проекта, различаясь только источником детекции.
		items = []NavItem{
			{LabelKey: "nav.errors", Href: "/projects/" + effID + "/issues"},
			{LabelKey: "nav.perf_issues", Href: "/projects/" + effID + "/perf-issues"},
			{LabelKey: "nav.regressions", Href: "/projects/" + effID + "/regressions"},
		}
		// Выгрузки ошибок (E1, задача 11) — та же граница CanOperate, что и
		// у остальных мутирующих подразделов ниже: exports сама по себе GET,
		// но встаёт в очередь тяжёлой выборки по ClickHouse — то же
		// требование requireProjectOperator, что у хендлеров
		// создания/скачивания/удаления заявки. ExportsEnabled — сверх
		// CanOperate: на инстансе без каталога выгрузок (h.Exports == nil)
		// пункт меню не показывается вовсе, а не ведёт на
		// страницу-объяснение (ревью веб-части E1, п.3).
		if s.CanOperate && s.ExportsEnabled {
			items = append(items,
				NavItem{LabelKey: "nav.exports", Href: "/projects/" + effID + "/exports"},
			)
		}
	case "performance":
		items = []NavItem{
			{LabelKey: "nav.transactions", Href: "/projects/" + effID + "/performance"},
			{LabelKey: "nav.webvitals", Href: "/projects/" + effID + "/web-vitals"},
			{LabelKey: "nav.profiles", Href: "/projects/" + effID + "/profiles"},
			{LabelKey: "nav.dependencies", Href: "/projects/" + effID + "/dependencies"},
			{LabelKey: "nav.deployments", Href: "/projects/" + effID + "/deployments"},
		}
	case "metrics":
		items = []NavItem{{LabelKey: "nav.metrics", Href: "/projects/" + effID + "/metrics"}}
	case "hosts":
		items = []NavItem{{LabelKey: "nav.hosts", Href: "/projects/" + effID + "/hosts"}}
	case "logs":
		// Просмотрщик логов (задача 2, C2): единственный подраздел —
		// список открыт всем с доступом к проекту, как и hosts выше (не
		// требует CanOperate — это не настройка, а чтение телеметрии).
		items = []NavItem{
			{LabelKey: "nav.logs", Href: "/projects/" + effID + "/logs"},
		}
	case "uptime":
		items = []NavItem{
			{LabelKey: "nav.monitors", Href: "/projects/" + effID + "/monitors"},
			{LabelKey: "nav.incidents", Href: "/projects/" + effID + "/incidents"},
		}
	case "alerts":
		// Вся конфигурация «когда меня позвать» собрана здесь из четырёх
		// прежних областей. Группы отвечают на три последовательных вопроса
		// настройки: при каком условии сработает → когда молчать → кому уйдёт.
		// Лента инцидентов переехала в «Обзор» (задача 7), поэтому у участника
		// без CanOperate область теперь схлопывается целиком — сознательное
		// решение спеки §4; просмотр правил на чтение отложен в 1.0+.
		if s.CanOperate {
			items = []NavItem{
				{Group: "nav.group.rules", LabelKey: "nav.rules_errors", Href: "/projects/" + effID + "/alerts"},
				{Group: "nav.group.rules", LabelKey: "nav.metric_alerts", Href: "/projects/" + effID + "/metrics/alerts"},
				{Group: "nav.group.rules", LabelKey: "nav.host_thresholds", Href: "/projects/" + effID + "/hosts/settings"},
				{Group: "nav.group.rules", LabelKey: "nav.slo", Href: "/projects/" + effID + "/slos"},
				{Group: "nav.group.silence", LabelKey: "nav.maintenance", Href: "/projects/" + effID + "/maintenance"},
				{Group: "nav.group.silence", LabelKey: "nav.alert_suppression", Href: "/projects/" + effID + "/alert-suppression"},
				{Group: "nav.group.delivery", LabelKey: "nav.escalations", Href: "/projects/" + effID + "/escalations"},
				{Group: "nav.group.delivery", LabelKey: "nav.alert_deliveries", Href: "/projects/" + effID + "/alerts/deliveries"},
			}
		}
	case "docs":
		// Doc page labels come from the markdown H1 (localized by
		// docs.Pages), not the i18n catalog — hence Label, not LabelKey.
		for _, p := range docs.Pages(s.Locale) {
			items = append(items, NavItem{Label: p.Title, Href: "/docs/" + p.Slug})
		}
	case "settings":
		// Две группы отвечают на разные вопросы: «Проект» — то, что
		// настраивает именно этот проект (доступно и участнику — рецепты и
		// шпаргалка установки читает любой с доступом), «Организация» —
		// управление организацией целиком, видна только owner/admin.
		if s.CanManage {
			items = append(items,
				NavItem{Group: "nav.group.project", LabelKey: "nav.project_settings", Href: "/projects/" + effID + "/settings"},
			)
		}
		if s.CanOperate {
			items = append(items,
				NavItem{Group: "nav.group.project", LabelKey: "nav.status_pages", Href: "/projects/" + effID + "/statuspages"},
			)
		}
		items = append(items,
			NavItem{Group: "nav.group.project", LabelKey: "nav.recipes", Href: "/projects/" + effID + "/recipes"},
			NavItem{Group: "nav.group.project", LabelKey: "getting_started.title", Href: "/projects/" + effID + "/setup"},
		)
		// Members/Teams/Probes are org-scoped management pages
		// (owner/admin only): without a resolved org id they would
		// link to /orgs/0/..., which 404s, and for a plain member
		// they 404 regardless of org id — so both a resolved org and
		// CanManage are required.
		if s.OrgID != 0 && s.CanManage {
			items = append(items,
				NavItem{Group: "nav.group.org", LabelKey: "nav.members", Href: "/orgs/" + orgID + "/settings"},
				NavItem{Group: "nav.group.org", LabelKey: "nav.teams", Href: "/orgs/" + orgID + "/teams"},
				NavItem{Group: "nav.group.org", LabelKey: "nav.probes", Href: "/orgs/" + orgID + "/probes"},
			)
		}
	default:
		return nil
	}

	markActive(items, activePath(s.Path, effID, s.Origin))
	return items
}

// AreaForOrigin — область рейла для подраздела-источника: подсветка области
// должна совпадать с подсветкой подраздела, иначе на трейсе, открытом из
// проблем производительности, светилась бы одна область, а пункт — из другой.
func AreaForOrigin(origin string) string {
	switch origin {
	case "web-vitals", "endpoint":
		return "performance"
	case "issue", "perf-issue":
		return "issues"
	default:
		return ""
	}
}

// activePath — путь, по которому ищется активный подраздел. Страницы-детали
// живут на корневых адресах без идентификатора проекта (/issues/{id},
// /perf-issues/{id}, /monitors/{id}, /traces/{id}), а пункты сайдбара имеют
// вид /projects/{id}/…, поэтому прямое сравнение не давало совпадений и на
// детали в сайдбаре не подсвечивалось НИЧЕГО: пользователь не видел, в каком
// разделе находится.
//
// Здесь корневой адрес приводится к списку своего раздела. Идентификатор
// проекта берётся тот же (effID), что и у ссылок сайдбара, иначе подсветка
// не совпала бы с ними.
func activePath(path, effID, origin string) string {
	// Источник перехода важнее пути: по одному адресу можно прийти из разных
	// разделов, и подсветка должна остаться там, откуда пришли — иначе она
	// спорит с хлебной крошкой.
	switch origin {
	case "web-vitals":
		return "/projects/" + effID + "/web-vitals"
	case "perf-issue":
		return "/projects/" + effID + "/perf-issues"
	case "issue":
		return "/projects/" + effID + "/issues"
	case "endpoint":
		return "/projects/" + effID + "/performance"
	}

	prefixes := []struct{ detail, list string }{
		{"/issues/", "/issues"},
		{"/perf-issues/", "/perf-issues"},
		{"/monitors/", "/monitors"},
		// Трейс принадлежит транзакции, поэтому подсвечиваем «Транзакции».
		{"/traces/", "/performance"},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p.detail) {
			return "/projects/" + effID + p.list
		}
	}
	return path
}

// markActive sets Active on the item whose Href is the longest prefix
// match of path, so that e.g. /projects/7/metrics/alerts activates the
// Metric Alerts item rather than the Metrics item.
func markActive(items []NavItem, path string) {
	if path == "" {
		return
	}
	best := -1
	for i, it := range items {
		if it.Href == "" || !strings.HasPrefix(path, it.Href) {
			continue
		}
		if best == -1 || len(it.Href) > len(items[best].Href) {
			best = i
		}
	}
	if best >= 0 {
		items[best].Active = true
	}
}

// Areas returns the icon-rail areas: overview first, then issues/
// performance/logs/metrics/hosts/uptime/alerts/settings, plus the trailing
// docs entry — each with Active set by s.Area and Href pointing at its
// first subsection for the effective project/org. Settings and docs carry
// Footer: true — they render in the rail's bottom tier, next to the avatar
// and logout.
func Areas(s Shell) []NavArea {
	result := make([]NavArea, 0, len(railAreas)+2)
	// Обзор (задача 6 nav-ia) — единственная область без подразделов:
	// контекстная колонка на ней не рендерится (Subsections отдаёт nil), а
	// значит firstSubsectionHref ниже вернул бы "" и область молча пропала
	// бы из рейла — тот же защитный приём, что убирает «Оповещения» у
	// участника без CanOperate, здесь бы стёр единственную область с явным
	// href. Поэтому href задаётся явно, а не через firstSubsectionHref, и
	// единственное условие показа — наличие проекта вообще.
	if effectiveProjectID(s) != 0 {
		result = append(result, NavArea{
			ID:       "overview",
			IconName: "home",
			LabelKey: "nav.overview",
			Href:     "/projects/" + itoa(effectiveProjectID(s)) + "/overview",
			Active:   s.Area == "overview",
		})
	}
	for _, a := range railAreas {
		href := firstSubsectionHref(s, a.id)
		// Область без единого доступного подраздела не показывается вовсе:
		// иначе иконка рейла вела бы участника на страницу, которая отдаёт ему
		// 404. Сейчас это «Оповещения» — обе её страницы требуют owner/admin.
		if href == "" {
			continue
		}
		result = append(result, NavArea{
			ID:       a.id,
			IconName: a.icon,
			LabelKey: a.labelKey,
			Href:     href,
			Active:   s.Area == a.id,
			Footer:   a.footer,
		})
	}

	// docs is visible to all roles and, unlike the other areas, always
	// points at the docs index (not the first subsection's href).
	result = append(result, NavArea{
		ID:       "docs",
		IconName: "book",
		LabelKey: "nav.docs",
		Href:     "/docs",
		Active:   s.Area == "docs",
		Footer:   true,
	})

	return result
}

// firstSubsectionHref computes the Href of the first subsection of area
// for the given shell's effective project/org, without regard to the
// shell's current Path/Area (Active is irrelevant here).
func firstSubsectionHref(s Shell, area string) string {
	probe := Shell{
		Projects:  s.Projects,
		ProjectID: s.ProjectID,
		OrgID:     s.OrgID,
		Area:      area,
		Locale:    s.Locale,
		// CanManage и CanOperate обязательны: подразделы фильтруются по
		// роли на обоих скоупах (организационном и проектном), и без них
		// область получала бы ссылку на страницу, закрытую для этого
		// человека — то есть ровно тот 404, от которого фильтр и заводится.
		CanManage:  s.CanManage,
		CanOperate: s.CanOperate,
	}
	subs := Subsections(probe)
	if len(subs) == 0 {
		return ""
	}
	return subs[0].Href
}

// ProjectSwitchHref computes where the project switcher takes the user when
// they pick projectID: the SAME area they are currently in (№60 — switching
// projects is not a reason to lose context), falling back to the project's
// overview when the current area is not a per-project one (overview itself,
// org, docs, settings) or has no accessible subsection for that project. The
// fallback used to be the issues list; the задача 6 nav-ia landing page is
// «Обзор», open to anyone with access to the project (same safety property
// the issues fallback had — see CanOperate note below), so a switch that
// can't stay in the current area now lands there instead. The probe keeps
// the shell's CanManage as-is: CanManage is per-organization, and the
// switcher only ever offers projects of the currently selected organization
// (topbar org selector, задача 4), so CanManage stays valid for the target.
//
// CanOperate is NOT carried over, and "alerts" is deliberately excluded from
// perProject below (задача 4, обязательный пункт: латентный дефект,
// зафиксированный в задаче 2). CanOperate is a per-PROJECT flag — the shell
// only computes it for the CURRENT project (see web/shell.go) — and team
// membership does not transfer between projects. "alerts" is the one area
// whose ENTIRE subsection list is gated behind CanOperate (см. Subsections
// case "alerts"): a stale CanOperate=true carried over from the current
// project could point the target project at a page that 404s there
// (requireProjectOperator), for a target where the user is not an operator.
// Rather than trust an unverified flag across the project boundary, the
// switcher always falls back to the target project's overview from
// "alerts" — a page open to anyone with access to the project, safe under
// any permission combination.
func ProjectSwitchHref(s Shell, projectID int64) string {
	perProject := map[string]bool{
		"issues": true, "performance": true, "metrics": true,
		"hosts": true, "logs": true, "uptime": true,
	}
	if perProject[s.Area] {
		target := s
		target.ProjectID = projectID
		if href := firstSubsectionHref(target, s.Area); href != "" {
			return href
		}
	}
	return "/projects/" + itoa(projectID) + "/overview"
}
