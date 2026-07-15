package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type metricAlertsStack struct {
	pool  *pgxpool.Pool
	srv   *httptest.Server
	org   *org.Service
	auth  *auth.Service
	rules *metric.RuleService
}

func newMetricAlertsStack(t *testing.T, wire bool) *metricAlertsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	rules := metric.NewRuleService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wire {
		h.MetricRules = rules
		h.MetricIncidents = metric.NewIncidentService(pool)
	}
	h.Register(mux)
	return &metricAlertsStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, rules: rules}
}

func TestWebMetricAlerts(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ma-co", "MA Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ma-proj", "MA Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"

	// Создание валидного правила (owner, с Origin) → 303, правило в списке.
	form := url.Values{
		"metric_name": {"http.errors"}, "aggregation": {"avg"}, "comparator": {"gt"},
		"threshold": {"100"}, "window_seconds": {"300"},
	}
	resp := postForm(t, s.srv, base, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", resp.StatusCode)
	}
	rules, _ := s.rules.List(ctx, project.ID)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}

	// Страница показывает правило.
	resp = getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "http.errors") {
		t.Fatalf("page missing rule (status %d): %s", resp.StatusCode, body)
	}

	// Невалидный порог → 422.
	bad := url.Values{"metric_name": {"m"}, "aggregation": {"avg"}, "comparator": {"gt"}, "threshold": {"nan!!"}, "window_seconds": {"300"}}
	resp = postForm(t, s.srv, base, bad, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad threshold status = %d, want 422", resp.StatusCode)
	}

	// Без Origin → 403.
	resp = postForm(t, s.srv, base, form, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}

	// Удаление правила.
	del := url.Values{"rule_id": {strconv.FormatInt(rules[0].ID, 10)}}
	resp = postForm(t, s.srv, base+"/delete", del, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	if rules, _ := s.rules.List(ctx, project.ID); len(rules) != 0 {
		t.Fatalf("rule not deleted")
	}

	// Member (не admin) → 404.
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "ma-member@example.com")
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	resp = getWithCookie(t, s.srv, base, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member status = %d, want 404", resp.StatusCode)
	}
}

func TestWebMetricAlertsNilService(t *testing.T) {
	s := newMetricAlertsStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "ma-nil-co", "MA Nil Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "ma-nil-proj", "MA Nil Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil service status = %d, want 404", resp.StatusCode)
	}
}
