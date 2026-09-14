package nav

import (
	"context"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

type Project struct {
	ID   int64
	Slug string
	Name string
	// Нужен web-слою, чтобы сужать список проектов пользователя до текущей
	// организации без похода в БД (см. web/shell.go).
	OrgID int64
}

type Org struct {
	ID   int64
	Name string
}

// LabelKey renders via i18n.T; Label, when set, renders directly instead —
// used where item labels come from content (e.g. docs H1), not the catalog.
type NavItem struct {
	LabelKey string
	Label    string
	Href     string
	Active   bool
	// Пустая строка — вне групп. Группа без ни одного видимого пункта исчезает
	// вместе с заголовком, а не остаётся пустой.
	Group string
}

// Пустой LabelKey — рендерить без заголовка.
type NavGroup struct {
	LabelKey string
	Items    []NavItem
}

func GroupedSubsections(s Shell) []NavGroup {
	return groupItems(Subsections(s))
}

// Объединяет только ПОДРЯД идущие пункты одной группы — группы, разбросанные
// по items не подряд, дадут два NavGroup с одинаковым LabelKey.
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

type NavArea struct {
	ID       string
	IconName string
	LabelKey string
	Href     string
	Active   bool
	// Рендерится в подвале рейла — ниже рабочих областей, рядом с аватаром и выходом.
	Footer bool
}

type Shell struct {
	UserEmail string
	Projects  []Project
	// Projects ниже уже сужены выбранной (OrgID) организацией (см. web/shell.go).
	Orgs      []Org
	ProjectID int64
	OrgID     int64
	Area      string
	Path      string
	// Подраздел-источник (?from=, провалидирован в web-слое) — нужен, когда один
	// адрес открывается из разных мест, иначе подсветка уезжает в чужой подраздел.
	Origin string
	// Used to build docs Subsections — titles come from localized markdown H1s
	// (internal/docs), not the i18n catalog.
	Locale string
	// Owner/admin of the current org; gates org-management links so plain
	// members never see links to pages that 404 for them.
	CanManage bool
	// Скоуп ПРОЕКТНЫЙ, не организационный: гейтит мониторинговые пункты
	// (requireProjectOperator), в отличие от CanManage (requireProjectRole).
	CanOperate bool
	// Фича сконфигурирована на инстансе (h.Exports != nil): без каталога воркер
	// не стартует, и пункт меню вёл бы на страницу, которая никогда не досчитается.
	ExportsEnabled bool
	// Фича сконфигурирована на инстансе (h.ProfileRegressions != nil): без
	// сервиса страница всегда отвечает 404, пункт меню на неё не нужен.
	ProfileRegressionsEnabled bool
	// Same-origin относительный путь из Referer (посчитан в web-слое); пусто,
	// если Referer отсутствует, ведёт на себя же или на чужой origin.
	Back string
}

type ctxKey struct{}

func WithShell(ctx context.Context, s Shell) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// Zero Shell if absent.
func FromContext(ctx context.Context) Shell {
	s, _ := ctx.Value(ctxKey{}).(Shell)
	return s
}

// Falls back to the first of s.Projects when ProjectID is unset.
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

// Три яруса, разделённые в вёрстке распорками: работа, настройка (alerts),
// служебное (settings). Порядок наблюдательных — по частоте обращения.
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

// Returns "" when the path does not belong to any area (project settings,
// setup, or unrecognized).
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
		// /orgs/{id}/projects и /profile осознанно не входят ни в одну область.
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
					// /metrics/alerts is a rule, grouped under alerts, not metrics.
					if len(parts) >= 3 && parts[2] == "alerts" {
						return "alerts"
					}
					return "metrics"
				case "hosts":
					// /hosts/settings configures thresholds — a rule, grouped under alerts.
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
					// /incident-feed redirects to overview (web/overview.go) — must
					// map to the same area, not disappear.
					return "overview"
				case "statuspages", "recipes", "settings", "setup":
					// Not a rail area, but owns the sidebar — without it the sidebar
					// would show only the project switcher.
					return "settings"
				}
			}
		}
	}
	return ""
}

// Пустая строка — путь не опознан, вызывающий подставляет общий "nav.back".
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
				return "nav.issues"
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
	// По конкретному пункту сайдбара, не по имени области — settings/teams/probes
	// все живут в одной области "settings".
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

func Subsections(s Shell) []NavItem {
	effID := itoa(effectiveProjectID(s))
	orgID := itoa(s.OrgID)

	var items []NavItem
	switch s.Area {
	case "issues":
		// Находки детекторов живут рядом со списком ошибок — все три читают один
		// поток проблем проекта, различаясь источником детекции.
		items = []NavItem{
			{LabelKey: "nav.issues", Href: "/projects/" + effID + "/issues"},
			{LabelKey: "nav.perf_issues", Href: "/projects/" + effID + "/perf-issues"},
			{LabelKey: "nav.regressions", Href: "/projects/" + effID + "/regressions"},
		}
		// CanOperate — та же граница, что у мутирующих хендлеров заявки; ExportsEnabled
		// сверх неё скрывает пункт совсем, когда каталог выгрузок не настроен.
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
		// Требует только ProfileRegressionsEnabled — сервис может быть не
		// сконфигурирован на инстансе.
		if s.ProfileRegressionsEnabled {
			items = append(items,
				NavItem{LabelKey: "nav.profile_regressions", Href: "/projects/" + effID + "/profile-regressions"},
			)
		}
	case "metrics":
		items = []NavItem{{LabelKey: "nav.metrics", Href: "/projects/" + effID + "/metrics"}}
	case "hosts":
		items = []NavItem{{LabelKey: "nav.hosts", Href: "/projects/" + effID + "/hosts"}}
	case "logs":
		// Открыт всем с доступом к проекту — это чтение телеметрии, не настройка.
		items = []NavItem{
			{LabelKey: "nav.logs", Href: "/projects/" + effID + "/logs"},
		}
	case "uptime":
		items = []NavItem{
			{LabelKey: "nav.monitors", Href: "/projects/" + effID + "/monitors"},
			{LabelKey: "nav.incidents", Href: "/projects/" + effID + "/incidents"},
		}
	case "alerts":
		// Три группы отвечают на «когда сработает → когда молчать → кому уйдёт».
		// Участник без CanOperate не видит область целиком — сознательное решение.
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
		// «Проект» — доступно участнику (рецепты, шпаргалка установки); «Организация»
		// — управление целиком, видна только owner/admin.
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
		// Both a resolved org and CanManage are required: OrgID==0 would link to
		// /orgs/0/..., and a plain member 404s regardless of org id.
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

// Должна совпадать с activePath: иначе подсветка области и подраздела
// разойдутся для одной и той же страницы.
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

// Детали живут на корневых адресах без id проекта (/issues/{id}…), а пункты
// сайдбара — на /projects/{id}/…: приводим первые ко вторым для подсветки.
func activePath(path, effID, origin string) string {
	// Источник важнее пути: подсветка должна остаться там, откуда пришли, иначе
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

// Longest-prefix match: /projects/7/metrics/alerts activates Metric Alerts,
// not Metrics.
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

func Areas(s Shell) []NavArea {
	result := make([]NavArea, 0, len(railAreas)+2)
	// firstSubsectionHref ниже вернул бы "" — у «Обзора» нет подразделов, и
	// область молча пропала бы из рейла, поэтому href задаётся явно.
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
		// Область без доступного подраздела не показывается — иначе иконка вела бы
		// на 404 (сейчас «Оповещения» — обе страницы требуют owner/admin).
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

	// Unlike other areas, always points at the docs index, not the first
	// subsection's href.
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

// Ignores the shell's current Path/Area — Active is irrelevant here.
func firstSubsectionHref(s Shell, area string) string {
	probe := Shell{
		Projects:  s.Projects,
		ProjectID: s.ProjectID,
		OrgID:     s.OrgID,
		Area:      area,
		Locale:    s.Locale,
		// CanManage/CanOperate обязательны: без них подразделы фильтровались бы как
		// у гостя, и область получила бы ссылку на закрытую страницу.
		CanManage:  s.CanManage,
		CanOperate: s.CanOperate,
	}
	subs := Subsections(probe)
	if len(subs) == 0 {
		return ""
	}
	return subs[0].Href
}

// CanOperate не переносится между проектами — поэтому "alerts" исключена
// из perProject: иначе цель могла получить 404 без CanOperate там.
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
