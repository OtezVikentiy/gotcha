package templates

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
)

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
