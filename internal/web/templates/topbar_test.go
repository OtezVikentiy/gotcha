package templates

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
)

// TestWhereAmIHidesAllProjectsDoorWithoutOrg — M2 финревью: OrgID
// резолвится best-effort в web/shell.go (ProjectOrg может ошибиться), и
// «Все проекты» — единственная дверь селекта проектов, ведущая на
// "/orgs/{id}/projects" — при OrgID==0 давала бы 404 ("/orgs/0/projects").
// Пункт должен рендериться только при разрешённой организации.
func TestWhereAmIHidesAllProjectsDoorWithoutOrg(t *testing.T) {
	shell := nav.Shell{
		Projects: []nav.Project{{ID: 7, Slug: "solo", Name: "Solo"}},
	}

	out := renderTo(t, whereAmI(shell))
	if strings.Contains(out, "/orgs/0/projects") {
		t.Errorf("OrgID==0 must not render a link to /orgs/0/projects (404): %s", out)
	}
	if strings.Contains(out, "proj-switch-all") {
		t.Errorf("OrgID==0 must hide the «Все проекты» door entirely, not just its href: %s", out)
	}

	shell.OrgID = 3
	out = renderTo(t, whereAmI(shell))
	if !strings.Contains(out, "/orgs/3/projects") {
		t.Errorf("resolved OrgID must still offer the «Все проекты» door: %s", out)
	}
}
