package web_test

import (
	"strings"
	"testing"
)

// authzLevel — задекларированный уровень прав маршрута (и мутирующего POST,
// и рендерящего GET). Сторож не проверяет реализацию гейта (это делают
// поведенческие тесты соответствующих ручек) — он не даёт ДОБАВИТЬ маршрут,
// не решив явно, кто имеет право его дёргать (урок живого кейса из спеки
// 2026-08-08: права раздавались по памяти, потому что карты не существовало).
//
// GET-маршруты добавлены в карту находкой B2 (arch P1-3): весь класс утечек,
// который правил этот аудит, живёт именно в GET-рендере страниц (не в POST),
// а карта до этой находки видела только мутации — новая GET-страница проекта
// без гейта добавлялась бы незамеченной.
const (
	lvlPublic   = "public"   // машинные/анонимные ручки (heartbeat, probe, login)
	lvlUser     = "user"     // любой залогиненный (профиль, logout, онбординг)
	lvlAccess   = "access"   // любой с доступом к проекту (статусы issue/perf-issue)
	lvlOperator = "operator" // оператор мониторинга (спека 2026-08-08)
	lvlAdmin    = "admin"    // org owner/admin
	lvlOwner    = "owner"    // только owner (удаления, SSO)
)

var routeAuthz = map[string]string{
	// --- Аутентификация и публичные ручки: без сессии, гейта нет по
	// построению (регистрируются в web.go ДО requireUser). ---
	"POST /login":           lvlPublic,
	"POST /register":        lvlPublic,
	"POST /logout":          lvlPublic,
	"POST /sso":             lvlPublic,
	"POST /settings/locale": lvlPublic, // доступен и анониму (например, на /login) — web.go
	"POST /settings/theme":  lvlPublic, // тот же принцип, что у locale

	// --- Машинные ручки: не браузер, аутентификация не сессией (токен
	// heartbeat в пути / Bearer пробы), только на стенде h.Uptime. ---
	"POST /uptime/hb/{token}": lvlPublic,
	"POST /probe/lease":       lvlPublic,
	"POST /probe/results":     lvlPublic,

	// --- Любой залогиненный: self-service над собственным аккаунтом, гейт —
	// только auth.UserID (нет дальнейшей проверки роли/доступа). ---
	"POST /profile/password":             lvlUser,
	"POST /profile/password/set":         lvlUser,
	"POST /profile/delete":               lvlUser,
	"POST /profile/sessions/revoke":      lvlUser,
	"POST /profile/identities/unlink":    lvlUser,
	"POST /profile/getting-started/hide": lvlUser,
	"POST /onboarding":                   lvlUser,
	"POST /issues/{id}/assign":           lvlAccess, // loadAccessibleIssue → CanAccessProject
	"POST /invite/{token}":               lvlUser,   // принять приглашение — адресат ещё не член
	// orgSettingsLeave: самостоятельный выход из организации — гейта роли
	// нет, RemoveMember(orgID, uid) сам проверяет членство (ErrNotMember →
	// 404); действие над собой, как остальные profile/* — lvlUser.
	"POST /orgs/{id}/settings/leave": lvlUser,

	// --- Доступ к проекту (CanAccessProject): issue/perf-issue статусы и
	// массовые операции — любой участник организации проекта. ---
	"POST /issues/{id}/status":        lvlAccess,
	"POST /projects/{id}/issues/bulk": lvlAccess,
	"POST /perf-issues/{id}/status":   lvlAccess,

	// --- Оператор мониторинга (requireProjectOperator, спека 2026-08-08):
	// мутации монитора, окон обслуживания, статус-страниц, alert rules и
	// metric alerts. ---
	"POST /monitors/{id}/pause":                                lvlOperator,
	"POST /monitors/{id}/resume":                               lvlOperator,
	"POST /monitors/{id}/delete":                               lvlOperator,
	"POST /monitors/{id}/heartbeat/regenerate":                 lvlOperator,
	"POST /monitors/{id}":                                      lvlOperator,
	"POST /projects/{id}/monitors":                             lvlOperator,
	"POST /projects/{id}/maintenance":                          lvlOperator,
	"POST /projects/{id}/maintenance/update":                   lvlOperator,
	"POST /projects/{id}/maintenance/delete":                   lvlOperator,
	"POST /projects/{id}/statuspages":                          lvlOperator,
	"POST /statuspages/{id}":                                   lvlOperator,
	"POST /statuspages/{id}/delete":                            lvlOperator,
	"POST /projects/{id}/alerts/rules":                         lvlOperator,
	"POST /projects/{id}/escalations":                          lvlOperator,
	"POST /projects/{id}/incidents/{source}/{incident_id}/ack": lvlOperator,
	"POST /projects/{id}/metrics/alerts":                       lvlOperator,
	"POST /projects/{id}/metrics/alerts/delete":                lvlOperator,
	"POST /projects/{id}/slos":                                 lvlOperator,
	"POST /projects/{id}/slos/{sloID}/delete":                  lvlOperator,
	"POST /projects/{id}/hosts/settings":                       lvlOperator,
	"POST /projects/{id}/hosts/settings/groups":                lvlOperator,
	"POST /projects/{id}/hosts/settings/groups/delete":         lvlOperator,
	"POST /projects/{id}/hosts/{name}/thresholds":              lvlOperator,
	"POST /projects/{id}/hosts/{name}/delete":                  lvlOperator,

	// --- Org admin/owner (requireOrgRole/requireProjectRole/requireTeamRole:
	// роль owner или admin в организации). ---
	"POST /projects/new":                         lvlAdmin, // projectCreate → requireOrgRole
	"POST /orgs/{id}/settings/role":              lvlAdmin,
	"POST /orgs/{id}/settings/remove":            lvlAdmin,
	"POST /orgs/{id}/settings/invite":            lvlAdmin,
	"POST /orgs/{id}/settings/invite/revoke":     lvlAdmin,
	"POST /orgs/{id}/settings/quota":             lvlAdmin,
	"POST /orgs/{id}/probes":                     lvlAdmin,
	"POST /orgs/{id}/probes/revoke":              lvlAdmin,
	"POST /orgs/{id}/teams":                      lvlAdmin,
	"POST /teams/{id}/rename":                    lvlAdmin,
	"POST /teams/{id}/members":                   lvlAdmin,
	"POST /teams/{id}/members/remove":            lvlAdmin,
	"POST /teams/{id}/projects":                  lvlAdmin,
	"POST /teams/{id}/projects/detach":           lvlAdmin,
	"POST /teams/{id}/delete":                    lvlAdmin,
	"POST /projects/{id}/settings/rename":        lvlAdmin,
	"POST /projects/{id}/settings/keys":          lvlAdmin,
	"POST /projects/{id}/settings/keys/revoke":   lvlAdmin,
	"POST /projects/{id}/settings/performance":   lvlAdmin,
	"POST /projects/{id}/settings/regressions":   lvlAdmin,
	"POST /projects/{id}/alerts/channels":        lvlAdmin,
	"POST /projects/{id}/alerts/channels/update": lvlAdmin,
	"POST /projects/{id}/alerts/channels/delete": lvlAdmin,
	"POST /projects/{id}/alerts/channels/test":   lvlAdmin,

	// --- Только owner (requireOrgOwner/requireProjectOwner/
	// requireInstanceAdminForSSO): удаления и SSO. ---
	"POST /orgs/{id}/settings/sso":            lvlOwner,
	"POST /orgs/{id}/settings/sso/delete":     lvlOwner,
	"POST /orgs/{id}/settings/delete":         lvlOwner,
	"POST /orgs/{id}/settings/purge-subject":  lvlOwner,
	"POST /orgs/{id}/settings/export-subject": lvlOwner,
	"POST /projects/{id}/settings/delete":     lvlOwner,

	// ============================= GET =============================
	// (находка B2: тот же принцип, что у POST выше, но для рендера).

	// --- Публичные GET: без сессии по построению — регистрируются в web.go
	// вне requireUser (статика, вход/регистрация/SSO, OAuth-старт/колбэк,
	// приглашение по токену — адресат ещё не член, heartbeat/status-страница —
	// машинные/анонимные ручки, см. блок h.Uptime != nil). ---
	"GET /login":                          lvlPublic,
	"GET /register":                       lvlPublic,
	"GET /sso":                            lvlPublic,
	"GET /invite/{token}":                 lvlPublic,
	"GET /auth/oauth/{provider}/start":    lvlPublic,
	"GET /auth/oauth/{provider}/callback": lvlPublic,
	"GET /static/":                        lvlPublic,
	"GET /uptime/hb/{token}":              lvlPublic,
	"GET /status/{key}":                   lvlPublic,
	"GET /install.sh":                     lvlPublic, // раздача агента (план A2) — машинная ручка, curl без сессии
	"GET /agent/{file}":                   lvlPublic,

	// --- Любой залогиненный (requireUser), дальше по коду гейта нет —
	// общие страницы аккаунта/продукта, не привязанные к конкретному
	// проекту/организации. ---
	"GET /{$}":         lvlUser,
	"GET /profile":     lvlUser,
	"GET /onboarding":  lvlUser,
	"GET /docs":        lvlUser,
	"GET /docs/{slug}": lvlUser,
	"GET /about":       lvlUser,
	"GET /projects":    lvlUser,

	// --- Доступ к проекту (CanAccessProject — напрямую или через
	// loadAccessibleIssue/loadAccessibleMonitor/loadAccessiblePerfIssue/
	// ProjectForTrace): просмотр открыт любому участнику организации
	// проекта, та же граница, что у issue/perf-issue статусов выше. ---
	"GET /projects/{id}/setup":                        lvlAccess,
	"GET /projects/{id}/issues":                       lvlAccess,
	"GET /issues/{id}":                                lvlAccess,
	"GET /projects/{id}/metrics":                      lvlAccess,
	"GET /projects/{id}/metrics/{name}":               lvlAccess,
	"GET /projects/{id}/hosts":                        lvlAccess,
	"GET /projects/{id}/hosts/{name}":                 lvlAccess,
	"GET /projects/{id}/logs":                         lvlAccess,
	"GET /projects/{id}/logs/attr-keys":               lvlAccess, // задача 6 (автокомплит): тот же гейт, что у самого списка логов
	"GET /projects/{id}/profiles":                     lvlAccess,
	"GET /projects/{id}/profiles/flame":               lvlAccess,
	"GET /projects/{id}/profile-regressions":          lvlAccess,
	"GET /projects/{id}/monitors":                     lvlAccess,
	"GET /monitors/{id}":                              lvlAccess,
	"GET /projects/{id}/incidents":                    lvlAccess,
	"GET /projects/{id}/performance":                  lvlAccess,
	"GET /projects/{id}/performance/{transaction...}": lvlAccess,
	"GET /projects/{id}/dependencies":                 lvlAccess,
	"GET /projects/{id}/web-vitals":                   lvlAccess,
	"GET /projects/{id}/perf-issues":                  lvlAccess,
	"GET /perf-issues/{id}":                           lvlAccess,
	"GET /projects/{id}/regressions":                  lvlAccess,
	"GET /projects/{id}/deployments":                  lvlAccess,
	"GET /traces/{trace_id}":                          lvlAccess,
	"GET /traces/{trace_id}/flame":                    lvlAccess,

	// --- Оператор мониторинга (requireProjectOperator): страницы алертов,
	// метрик-алертов, форм монитора, статус-страниц и окон обслуживания —
	// тот же гейт, что у их мутирующих POST в разделе operator выше.
	// ВНИМАНИЕ (потенциальная находка для контроллера): комментарий в web.go
	// у /projects/{id}/statuspages и /projects/{id}/maintenance называет их
	// уровнем requireProjectRole (owner/admin) — это устарело, фактический
	// гейт в коде обеих GET-ручек (statusPagesPage, maintenancePage) и всех
	// их POST — requireProjectOperator; карта отражает код, а не комментарий.
	"GET /projects/{id}/metrics/alerts":    lvlOperator,
	"GET /projects/{id}/slos":              lvlOperator,
	"GET /projects/{id}/slos/{sloID}":      lvlOperator,
	"GET /projects/{id}/hosts/settings":    lvlOperator,
	"GET /projects/{id}/alerts":            lvlOperator,
	"GET /projects/{id}/alerts/deliveries": lvlOperator,
	"GET /projects/{id}/escalations":       lvlOperator,
	"GET /projects/{id}/monitors/new":      lvlOperator,
	"GET /monitors/{id}/edit":              lvlOperator,
	"GET /projects/{id}/statuspages":       lvlOperator,
	"GET /projects/{id}/maintenance":       lvlOperator,

	// --- Org admin/owner (requireOrgRole/requireProjectRole): настройки
	// организации/проекта и производные разделы (пробы, команды) — та же
	// граница, что у мутирующих POST в соответствующем разделе выше. ---
	"GET /orgs/{id}/settings":     lvlAdmin,
	"GET /orgs/{id}/probes":       lvlAdmin,
	"GET /orgs/{id}/teams":        lvlAdmin,
	"GET /projects/{id}/settings": lvlAdmin,
}

func TestRoutesDeclareAuthzLevel(t *testing.T) {
	s := newUptimeStack(t)
	declared := make(map[string]bool, len(routeAuthz))
	for route, lvl := range routeAuthz {
		declared[route] = false
		switch lvl {
		case lvlPublic, lvlUser, lvlAccess, lvlOperator, lvlAdmin, lvlOwner:
		default:
			t.Errorf("маршрут %q: неизвестный уровень %q", route, lvl)
		}
	}
	for _, route := range s.h.RegisteredRoutes() {
		if !strings.HasPrefix(route, "GET ") && !strings.HasPrefix(route, "POST ") {
			continue
		}
		if _, ok := routeAuthz[route]; !ok {
			t.Errorf("маршрут %q не отнесён к уровню прав — добавь его в routeAuthz, решив, кто имеет право (см. спеку 2026-08-08)", route)
			continue
		}
		declared[route] = true
	}
	for route, seen := range declared {
		if !seen {
			t.Errorf("карта прав упоминает %q, но такого маршрута больше нет — удали запись", route)
		}
	}
}
