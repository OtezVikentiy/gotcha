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
}

// NavArea is a single icon-rail entry.
type NavArea struct {
	ID       string
	IconName string
	LabelKey string
	Href     string
	Active   bool
}

// Shell is the app-shell state carried through the request context: the
// current user, their projects (for the switcher), the current
// project/org, the active rail area, and the current request path (used
// to compute Active on nav items).
type Shell struct {
	UserEmail string
	Projects  []Project
	ProjectID int64
	OrgID     int64
	Area      string
	OrgMode   bool
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

// railAreas is the static, ordered definition of the icon-rail areas
// (excluding the org area, which is a separate bottom rail entry).
var railAreas = []struct {
	id, icon, labelKey string
}{
	{"issues", "bug", "nav.issues"},
	{"performance", "zap", "nav.performance"},
	{"metrics", "chart", "nav.metrics"},
	{"uptime", "activity", "nav.uptime"},
	{"alerts", "bell", "nav.alerts"},
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
	case strings.HasPrefix(path, "/traces/"), strings.HasPrefix(path, "/perf-issues/"):
		return "performance"
	case strings.HasPrefix(path, "/monitors/"), strings.HasPrefix(path, "/statuspages/"):
		return "uptime"
	case strings.HasPrefix(path, "/orgs/"):
		return "org"
	case path == "/projects":
		return "org"
	case path == "/profile":
		return "org"
	case path == "/setup", strings.HasPrefix(path, "/setup/"):
		return ""
	}

	if strings.HasPrefix(path, "/projects/") {
		rest := strings.TrimPrefix(path, "/projects/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 {
			if _, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				switch parts[1] {
				case "issues":
					return "issues"
				case "performance", "web-vitals", "profiles", "profile-regressions", "perf-issues", "regressions":
					return "performance"
				case "metrics":
					return "metrics"
				case "monitors", "incidents", "maintenance", "statuspages":
					return "uptime"
				case "alerts":
					return "alerts"
				case "settings", "setup":
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

// Subsections returns the sidebar subsections for s.Area, scoped to the
// effective project/org, with Active set for the item whose Href is the
// longest prefix match of s.Path.
func Subsections(s Shell) []NavItem {
	effID := itoa(effectiveProjectID(s))
	orgID := itoa(s.OrgID)

	var items []NavItem
	switch s.Area {
	case "issues":
		items = []NavItem{
			{LabelKey: "nav.issues", Href: "/projects/" + effID + "/issues"},
		}
	case "performance":
		items = []NavItem{
			{LabelKey: "nav.transactions", Href: "/projects/" + effID + "/performance"},
			{LabelKey: "nav.webvitals", Href: "/projects/" + effID + "/web-vitals"},
			{LabelKey: "nav.profiles", Href: "/projects/" + effID + "/profiles"},
			{LabelKey: "nav.perf_issues", Href: "/projects/" + effID + "/perf-issues"},
			{LabelKey: "nav.regressions", Href: "/projects/" + effID + "/regressions"},
		}
	case "metrics":
		items = []NavItem{
			{LabelKey: "nav.metrics", Href: "/projects/" + effID + "/metrics"},
			{LabelKey: "nav.metric_alerts", Href: "/projects/" + effID + "/metrics/alerts"},
		}
	case "uptime":
		items = []NavItem{
			{LabelKey: "nav.monitors", Href: "/projects/" + effID + "/monitors"},
			{LabelKey: "nav.incidents", Href: "/projects/" + effID + "/incidents"},
			{LabelKey: "nav.maintenance", Href: "/projects/" + effID + "/maintenance"},
			{LabelKey: "nav.status_pages", Href: "/projects/" + effID + "/statuspages"},
		}
	case "alerts":
		items = []NavItem{
			{LabelKey: "nav.alerts", Href: "/projects/" + effID + "/alerts"},
			{LabelKey: "nav.alert_deliveries", Href: "/projects/" + effID + "/alerts/deliveries"},
		}
	case "docs":
		// Doc page labels come from the markdown H1 (localized by
		// docs.Pages), not the i18n catalog — hence Label, not LabelKey.
		for _, p := range docs.Pages(s.Locale) {
			items = append(items, NavItem{Label: p.Title, Href: "/docs/" + p.Slug})
		}
	case "settings":
		items = []NavItem{
			{LabelKey: "nav.project_settings", Href: "/projects/" + effID + "/settings"},
			{LabelKey: "getting_started.title", Href: "/projects/" + effID + "/setup"},
		}
	case "org":
		items = []NavItem{
			{LabelKey: "nav.projects", Href: "/projects"},
		}
		// Members/Teams/Probes are org-scoped management pages
		// (owner/admin only): without a resolved org id they would
		// link to /orgs/0/..., which 404s, and for a plain member
		// they 404 regardless of org id — so both a resolved org and
		// CanManage are required.
		if s.OrgID != 0 && s.CanManage {
			items = append(items,
				NavItem{LabelKey: "nav.members", Href: "/orgs/" + orgID + "/settings"},
				NavItem{LabelKey: "nav.teams", Href: "/orgs/" + orgID + "/teams"},
				NavItem{LabelKey: "nav.probes", Href: "/orgs/" + orgID + "/probes"},
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
	case "web-vitals", "perf-issue", "endpoint":
		return "performance"
	case "issue":
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

// Areas returns the icon-rail areas (issues/performance/metrics/uptime/
// alerts, plus the trailing org entry), each with Active set by s.Area
// and Href pointing at its first subsection for the effective
// project/org.
func Areas(s Shell) []NavArea {
	result := make([]NavArea, 0, len(railAreas)+1)
	for _, a := range railAreas {
		href := firstSubsectionHref(s, a.id)
		result = append(result, NavArea{
			ID:       a.id,
			IconName: a.icon,
			LabelKey: a.labelKey,
			Href:     href,
			Active:   s.Area == a.id,
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
	})

	orgHref := firstSubsectionHref(s, "org")
	result = append(result, NavArea{
		ID:       "org",
		IconName: "building",
		LabelKey: "nav.org",
		Href:     orgHref,
		Active:   s.Area == "org",
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
	}
	subs := Subsections(probe)
	if len(subs) == 0 {
		return ""
	}
	return subs[0].Href
}
