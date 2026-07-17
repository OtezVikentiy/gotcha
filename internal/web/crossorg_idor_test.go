package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// Cross-org IDOR (audit #3 / M12). A user who is owner of org A, with ZERO
// membership in org B, must get 404 (uniform-existence-oracle: not 403) on
// every attempt to READ or MUTATE a resource in org B — and no mutation may
// occur. These tests differ from the existing same-org member→404 tests: the
// actor here is a total stranger to the target org, proving the guard is
// org-scoped, not merely role-scoped.

// crossOrgVictimProject builds two orgs: an attacker who owns org A (and is not
// a member of org B in any capacity) and a victim project inside org B. Returns
// the attacker's session cookie and the victim project.
func crossOrgVictimProject(t *testing.T, authSvc *auth.Service, orgSvc *org.Service, prefix string) (*http.Cookie, org.Project) {
	t.Helper()
	attackerID, attackerCookie := orgSettingsRegister(t, authSvc, prefix+"-attacker@example.com")
	victimID, _ := orgSettingsRegister(t, authSvc, prefix+"-victim@example.com")

	// Attacker owns org A — a full owner, but only of their OWN org.
	if _, err := orgSvc.CreateOrg(context.Background(), prefix+"-attacker-co", prefix+" Attacker Co", attackerID); err != nil {
		t.Fatalf("create attacker org: %v", err)
	}
	// Victim owns org B with the target project. Attacker has no membership here.
	orgB, err := orgSvc.CreateOrg(context.Background(), prefix+"-victim-co", prefix+" Victim Co", victimID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), orgB.ID, prefix+"-victim-proj", prefix+" Victim Proj", "go")
	if err != nil {
		t.Fatalf("create victim project: %v", err)
	}
	return attackerCookie, proj
}

func statusOf(t *testing.T, resp *http.Response) int {
	t.Helper()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// TestWebCrossOrgProjectIDOR — project settings (read + rename), alerts (read)
// and project API-key operations (create + revoke) are all 404 for a
// cross-org owner, and none of them mutate.
func TestWebCrossOrgProjectIDOR(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	attackerCookie, proj := crossOrgVictimProject(t, authSvc, orgSvc, "idor-proj")

	// Seed a live key in the victim project so revoke has a real target.
	victimKey, err := orgSvc.CreateKey(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("create victim key: %v", err)
	}

	settings := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	origName := proj.Name

	// READ settings → 404.
	if code := statusOf(t, getWithCookie(t, s.srv, settings, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("GET %s (cross-org) = %d, want 404", settings, code)
	}
	// READ alerts → 404.
	alerts := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	if code := statusOf(t, getWithCookie(t, s.srv, alerts, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("GET %s (cross-org) = %d, want 404", alerts, code)
	}

	// MUTATE rename → 404, name unchanged.
	if code := statusOf(t, postForm(t, s.srv, settings+"/rename",
		url.Values{"name": {"Pwned"}}, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST rename (cross-org) = %d, want 404", code)
	}
	if got, _ := orgSvc.GetProject(context.Background(), proj.ID); got.Name != origName {
		t.Errorf("project renamed cross-org: %q, want unchanged %q", got.Name, origName)
	}

	// MUTATE key create → 404, no new key.
	if code := statusOf(t, postForm(t, s.srv, settings+"/keys",
		url.Values{}, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST keys create (cross-org) = %d, want 404", code)
	}
	if keys, _ := orgSvc.KeysForProject(context.Background(), proj.ID); len(keys) != 1 {
		t.Errorf("victim keys after cross-org create = %d, want 1 (no new key)", len(keys))
	}

	// MUTATE key revoke of the victim's key → 404, key not revoked.
	if code := statusOf(t, postForm(t, s.srv, settings+"/keys/revoke",
		url.Values{"key_id": {strconv.FormatInt(victimKey.ID, 10)}, "confirmed": {"yes"}},
		s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST keys revoke (cross-org) = %d, want 404", code)
	}
	keys, _ := orgSvc.KeysForProject(context.Background(), proj.ID)
	if len(keys) != 1 || keys[0].Revoked {
		t.Errorf("victim key revoked cross-org: %+v", keys)
	}
}

// TestWebCrossOrgMetricAlertIDOR — metric alert rules (page read, create,
// delete) are 404 for a cross-org owner, and neither create nor delete mutate.
func TestWebCrossOrgMetricAlertIDOR(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	attackerCookie, proj := crossOrgVictimProject(t, s.auth, s.org, "idor-ma")

	// Seed a victim rule so delete has a real target.
	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/alerts"
	if _, err := s.rules.Create(context.Background(), metric.Rule{
		ProjectID: proj.ID, MetricName: "victim.metric", Aggregation: "avg",
		Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true,
	}); err != nil {
		t.Fatalf("seed metric rule: %v", err)
	}
	rules, _ := s.rules.List(context.Background(), proj.ID)
	if len(rules) != 1 {
		t.Fatalf("seed rule failed, rules=%d", len(rules))
	}

	// READ page → 404.
	if code := statusOf(t, getWithCookie(t, s.srv, base, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("GET %s (cross-org) = %d, want 404", base, code)
	}
	// CREATE → 404, still 1 rule.
	create := url.Values{"metric_name": {"evil"}, "aggregation": {"avg"}, "comparator": {"gt"}, "threshold": {"1"}, "window_seconds": {"300"}}
	if code := statusOf(t, postForm(t, s.srv, base, create, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST create (cross-org) = %d, want 404", code)
	}
	if got, _ := s.rules.List(context.Background(), proj.ID); len(got) != 1 {
		t.Errorf("rules after cross-org create = %d, want 1", len(got))
	}
	// DELETE the victim's real rule → 404, rule still present.
	del := url.Values{"rule_id": {strconv.FormatInt(rules[0].ID, 10)}}
	if code := statusOf(t, postForm(t, s.srv, base+"/delete", del, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST delete (cross-org) = %d, want 404", code)
	}
	if got, _ := s.rules.List(context.Background(), proj.ID); len(got) != 1 {
		t.Errorf("rules after cross-org delete = %d, want 1 (not deleted)", len(got))
	}
}

// TestWebCrossOrgMonitorIDOR — a monitor addressed by its global id
// (/monitors/{id}) is 404 for a cross-org owner on read (detail, edit) and on
// mutation (delete), and the monitor survives.
func TestWebCrossOrgMonitorIDOR(t *testing.T) {
	s := newMonitorFormStack(t)
	attackerCookie, proj := crossOrgVictimProject(t, s.auth, s.org, "idor-mon")

	// Seed a victim heartbeat monitor.
	hbCfg, _ := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	victim, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID:         proj.ID,
		Name:              "victim monitor",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            hbCfg,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create victim monitor: %v", err)
	}
	mon := "/monitors/" + strconv.FormatInt(victim.ID, 10)

	// READ detail → 404.
	if code := statusOf(t, getWithCookie(t, s.srv, mon, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("GET %s (cross-org) = %d, want 404", mon, code)
	}
	// READ edit form → 404.
	if code := statusOf(t, getWithCookie(t, s.srv, mon+"/edit", attackerCookie)); code != http.StatusNotFound {
		t.Errorf("GET %s/edit (cross-org) = %d, want 404", mon, code)
	}
	// MUTATE delete → 404, monitor survives.
	if code := statusOf(t, postForm(t, s.srv, mon+"/delete", url.Values{}, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST %s/delete (cross-org) = %d, want 404", mon, code)
	}
	if _, err := s.uptime.Get(context.Background(), victim.ID); err != nil {
		t.Errorf("victim monitor deleted cross-org: Get err = %v", err)
	}

	// MUTATE create under the victim project → 404, no monitor added.
	before, _ := s.uptime.List(context.Background(), proj.ID)
	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	if code := statusOf(t, postForm(t, s.srv, createPath,
		url.Values{"kind": {"heartbeat"}, "name": {"evil"}}, s.srv.URL, attackerCookie)); code != http.StatusNotFound {
		t.Errorf("POST %s (cross-org create) = %d, want 404", createPath, code)
	}
	after, _ := s.uptime.List(context.Background(), proj.ID)
	if len(after) != len(before) {
		t.Errorf("victim project monitors after cross-org create = %d, want %d", len(after), len(before))
	}
}
