package web_test

import (
	"strings"
	"testing"
)

// authzLevel — задекларированный уровень прав мутирующего маршрута.
// Сторож не проверяет реализацию гейта (это делают поведенческие тесты
// соответствующих ручек) — он не даёт ДОБАВИТЬ маршрут, не решив явно,
// кто имеет право его дёргать (урок живого кейса из спеки 2026-08-08:
// права раздавались по памяти, потому что карты не существовало).
const (
	lvlPublic   = "public"   // машинные/анонимные ручки (heartbeat, probe, login)
	lvlUser     = "user"     // любой залогиненный (профиль, logout, онбординг)
	lvlAccess   = "access"   // любой с доступом к проекту (статусы issue/perf-issue)
	lvlOperator = "operator" // оператор мониторинга (спека 2026-08-08)
	lvlAdmin    = "admin"    // org owner/admin
	lvlOwner    = "owner"    // только owner (удаления, SSO)
)

var mutatingRouteAuthz = map[string]string{
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
	"POST /monitors/{id}/pause":                 lvlOperator,
	"POST /monitors/{id}/resume":                lvlOperator,
	"POST /monitors/{id}/delete":                lvlOperator,
	"POST /monitors/{id}/heartbeat/regenerate":  lvlOperator,
	"POST /monitors/{id}":                       lvlOperator,
	"POST /projects/{id}/monitors":              lvlOperator,
	"POST /projects/{id}/maintenance":           lvlOperator,
	"POST /projects/{id}/maintenance/update":    lvlOperator,
	"POST /projects/{id}/maintenance/delete":    lvlOperator,
	"POST /projects/{id}/statuspages":           lvlOperator,
	"POST /statuspages/{id}":                    lvlOperator,
	"POST /statuspages/{id}/delete":             lvlOperator,
	"POST /projects/{id}/alerts/rules":          lvlOperator,
	"POST /projects/{id}/metrics/alerts":        lvlOperator,
	"POST /projects/{id}/metrics/alerts/delete": lvlOperator,

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
}

func TestMutatingRoutesDeclareAuthzLevel(t *testing.T) {
	s := newUptimeStack(t)
	declared := make(map[string]bool, len(mutatingRouteAuthz))
	for route, lvl := range mutatingRouteAuthz {
		declared[route] = false
		switch lvl {
		case lvlPublic, lvlUser, lvlAccess, lvlOperator, lvlAdmin, lvlOwner:
		default:
			t.Errorf("маршрут %q: неизвестный уровень %q", route, lvl)
		}
	}
	for _, route := range s.h.RegisteredRoutes() {
		if !strings.HasPrefix(route, "POST ") {
			continue
		}
		if _, ok := mutatingRouteAuthz[route]; !ok {
			t.Errorf("мутирующий маршрут %q не отнесён к уровню прав — добавь его в mutatingRouteAuthz, решив, кто имеет право (см. спеку 2026-08-08)", route)
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
