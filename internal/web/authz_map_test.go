package web_test

import (
	"strings"
	"testing"
)

const (
	lvlPublic        = "public"
	lvlUser          = "user"
	lvlAccess        = "access"
	lvlOperator      = "operator"
	lvlAdmin         = "admin"
	lvlOwner         = "owner"
	lvlInstanceAdmin = "instance_admin"
)

var routeAuthz = map[string]string{
	"POST /login":           lvlPublic,
	"POST /register":        lvlPublic,
	"POST /logout":          lvlPublic,
	"POST /sso":             lvlPublic,
	"POST /settings/locale": lvlPublic,
	"POST /settings/theme":  lvlPublic,

	"POST /uptime/hb/{token}": lvlPublic,
	"POST /probe/lease":       lvlPublic,
	"POST /probe/results":     lvlPublic,

	"POST /profile/password":             lvlUser,
	"POST /profile/password/set":         lvlUser,
	"POST /profile/delete":               lvlUser,
	"POST /profile/sessions/revoke":      lvlUser,
	"POST /profile/identities/unlink":    lvlUser,
	"POST /profile/getting-started/hide": lvlUser,
	"POST /onboarding":                   lvlUser,
	"POST /issues/{id}/assign":           lvlAccess,
	"POST /invite/{token}":               lvlUser,
	"POST /orgs/{id}/settings/leave":     lvlUser,

	"POST /issues/{id}/status":        lvlAccess,
	"POST /projects/{id}/issues/bulk": lvlAccess,
	"POST /perf-issues/{id}/status":   lvlAccess,

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
	"POST /projects/{id}/alert-suppression":                    lvlOperator,
	"POST /projects/{id}/alert-suppression/{depID}":            lvlOperator,
	"POST /projects/{id}/alert-suppression/{depID}/delete":     lvlOperator,
	"POST /projects/{id}/exports":                              lvlOperator,
	"POST /projects/{id}/exports/{jobID}/delete":               lvlOperator,
	"POST /projects/{id}/incidents/{source}/{incident_id}/ack": lvlOperator,
	"POST /projects/{id}/metrics/alerts":                       lvlOperator,
	"POST /projects/{id}/metrics/alerts/delete":                lvlOperator,
	"POST /projects/{id}/metrics/alerts/{ruleID}":              lvlOperator,
	"POST /projects/{id}/recipes/{slug}/thresholds":            lvlOperator,
	"POST /projects/{id}/slos":                                 lvlOperator,
	"POST /projects/{id}/slos/{sloID}/delete":                  lvlOperator,
	"POST /projects/{id}/hosts/settings":                       lvlOperator,
	"POST /projects/{id}/hosts/settings/groups":                lvlOperator,
	"POST /projects/{id}/hosts/settings/groups/delete":         lvlOperator,
	"POST /projects/{id}/hosts/{name}/thresholds":              lvlOperator,
	"POST /projects/{id}/hosts/{name}/delete":                  lvlOperator,

	"POST /projects/new":                         lvlAdmin,
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

	"POST /orgs/{id}/settings/delete":         lvlOwner,
	"POST /orgs/{id}/settings/purge-subject":  lvlOwner,
	"POST /orgs/{id}/settings/export-subject": lvlOwner,
	"POST /projects/{id}/settings/delete":     lvlOwner,

	"POST /orgs/{id}/settings/sso":          lvlInstanceAdmin,
	"POST /orgs/{id}/settings/sso/delete":   lvlInstanceAdmin,
	"POST /profile/instance-admin/transfer": lvlInstanceAdmin,

	"GET /login":                          lvlPublic,
	"GET /register":                       lvlPublic,
	"GET /sso":                            lvlPublic,
	"GET /invite/{token}":                 lvlPublic,
	"GET /auth/oauth/{provider}/start":    lvlPublic,
	"GET /auth/oauth/{provider}/callback": lvlPublic,
	"GET /static/":                        lvlPublic,
	"GET /uptime/hb/{token}":              lvlPublic,
	"GET /status/{key}":                   lvlPublic,
	"GET /install.sh":                     lvlPublic,
	"GET /agent/{file}":                   lvlPublic,

	"GET /{$}":                lvlUser,
	"GET /profile":            lvlUser,
	"GET /onboarding":         lvlUser,
	"GET /docs":               lvlUser,
	"GET /docs/{slug}":        lvlUser,
	"GET /about":              lvlUser,
	"GET /projects":           lvlUser,
	"GET /orgs/{id}/projects": lvlUser,

	"GET /projects/{id}/setup":                            lvlAccess,
	"GET /projects/{id}/issues":                           lvlAccess,
	"GET /issues/{id}":                                    lvlAccess,
	"GET /projects/{id}/metrics":                          lvlAccess,
	"GET /projects/{id}/metrics/{name}":                   lvlAccess,
	"GET /projects/{id}/recipes":                          lvlAccess,
	"GET /projects/{id}/recipes/{slug}":                   lvlAccess,
	"GET /projects/{id}/hosts":                            lvlAccess,
	"GET /projects/{id}/hosts/{name}":                     lvlAccess,
	"GET /projects/{id}/logs":                             lvlAccess,
	"GET /projects/{id}/logs/attr-keys":                   lvlAccess,
	"POST /projects/{id}/logs/filters":                    lvlAccess,
	"POST /projects/{id}/logs/filters/{filterID}/update":  lvlAccess,
	"POST /projects/{id}/logs/filters/{filterID}/delete":  lvlAccess,
	"POST /projects/{id}/logs/filters/{filterID}/default": lvlAccess,
	"GET /projects/{id}/profiles":                         lvlAccess,
	"GET /projects/{id}/profiles/flame":                   lvlAccess,
	"GET /projects/{id}/profile-regressions":              lvlAccess,
	"GET /projects/{id}/monitors":                         lvlAccess,
	"GET /monitors/{id}":                                  lvlAccess,
	"GET /projects/{id}/incidents":                        lvlAccess,
	"GET /projects/{id}/overview":                         lvlAccess,
	"GET /projects/{id}/incident-feed":                    lvlAccess,
	"GET /projects/{id}/performance":                      lvlAccess,
	"GET /projects/{id}/performance/{transaction...}":     lvlAccess,
	"GET /projects/{id}/dependencies":                     lvlAccess,
	"GET /projects/{id}/web-vitals":                       lvlAccess,
	"GET /projects/{id}/perf-issues":                      lvlAccess,
	"GET /perf-issues/{id}":                               lvlAccess,
	"GET /projects/{id}/regressions":                      lvlAccess,
	"GET /projects/{id}/deployments":                      lvlAccess,
	"GET /traces/{trace_id}":                              lvlAccess,
	"GET /traces/{trace_id}/flame":                        lvlAccess,

	"GET /projects/{id}/metrics/alerts":           lvlOperator,
	"GET /projects/{id}/slos":                     lvlOperator,
	"GET /projects/{id}/slos/{sloID}":             lvlOperator,
	"GET /projects/{id}/hosts/settings":           lvlOperator,
	"GET /projects/{id}/alerts":                   lvlOperator,
	"GET /projects/{id}/alerts/deliveries":        lvlOperator,
	"GET /projects/{id}/escalations":              lvlOperator,
	"GET /projects/{id}/alert-suppression":        lvlOperator,
	"GET /projects/{id}/exports":                  lvlOperator,
	"GET /projects/{id}/exports/{jobID}/download": lvlOperator,
	"GET /projects/{id}/monitors/new":             lvlOperator,
	"GET /monitors/{id}/edit":                     lvlOperator,
	"GET /projects/{id}/statuspages":              lvlOperator,
	"GET /projects/{id}/maintenance":              lvlOperator,

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
		case lvlPublic, lvlUser, lvlAccess, lvlOperator, lvlAdmin, lvlOwner, lvlInstanceAdmin:
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
