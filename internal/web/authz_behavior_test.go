package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/guards"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

var strangerScopedPrefixes = []string{
	"/projects/{id}",
	"/orgs/{id}",
	"/monitors/{id}",
	"/statuspages/{id}",
	"/teams/{id}",
	"/perf-issues/{id}",
	"/issues/{id}",
}

func isStrangerScopedRoute(path string) bool {
	for _, p := range strangerScopedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

var leaveConfirmExemption = []guards.Exemption{
	{
		Value:   "POST /orgs/{id}/settings/leave",
		Why:     "экран подтверждения общий для всех и не палит организацию; настоящая мутация (confirmed=yes) отдельно проверена на 404 ниже",
		Finding: "B3",
	},
}

type victimIDs struct {
	orgID        int64
	projectID    int64
	monitorID    int64
	statuspageID int64
	teamID       int64
	issueID      int64
	perfIssueID  int64
}

func concreteVictimPath(t *testing.T, path string, v victimIDs) string {
	t.Helper()
	switch {
	case strings.HasPrefix(path, "/projects/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.projectID, 10), 1)
	case strings.HasPrefix(path, "/orgs/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.orgID, 10), 1)
	case strings.HasPrefix(path, "/monitors/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.monitorID, 10), 1)
	case strings.HasPrefix(path, "/statuspages/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.statuspageID, 10), 1)
	case strings.HasPrefix(path, "/teams/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.teamID, 10), 1)
	case strings.HasPrefix(path, "/perf-issues/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.perfIssueID, 10), 1)
	case strings.HasPrefix(path, "/issues/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.issueID, 10), 1)
	default:
		t.Fatalf("маршрут %q не подпадает ни под один известный тип ресурса — обнови concreteVictimPath/strangerScopedPrefixes", path)
	}
	return routePlaceholder.ReplaceAllString(path, "x")
}

func TestAuthzBehaviorStrangerRejectedOnScopedRoutes(t *testing.T) {
	s := newUptimeStack(t)
	ch := testenv.MigratedCH(t)

	s.h.Alerts = alert.NewService(s.pool)
	s.h.Outbox = notify.NewOutbox(s.pool)
	s.h.NotifyDirect = &notify.Direct{}
	s.h.UptimeQuery = uptime.NewQuery(ch)
	s.h.Trace = trace.NewQuery(ch)
	s.h.PerfIssues = trace.NewIssueService(s.pool)
	s.h.Regressions = trace.NewRegressionService(s.pool)
	s.h.Metrics = metric.NewQuery(ch)
	s.h.MetricRules = metric.NewRuleService(s.pool)
	s.h.MetricIncidents = metric.NewIncidentService(s.pool)
	s.h.Profiles = profile.NewQuery(ch)
	s.h.ProfileRegressions = profile.NewRegressionService(s.pool)

	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	orgSettingsRegister(t, authSvc, "b3-bootstrap-sink@example.com")

	strangerID, strangerCookie := orgSettingsRegister(t, authSvc, "b3-stranger@example.com")
	if _, err := orgSvc.CreateOrg(ctx, "b3-stranger-co", "B3 Stranger Co", strangerID); err != nil {
		t.Fatalf("create stranger org: %v", err)
	}

	victimOwnerID, _ := orgSettingsRegister(t, authSvc, "b3-victim@example.com")
	victimOrg, err := orgSvc.CreateOrg(ctx, "b3-victim-co", "B3 Victim Co", victimOwnerID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	victimProject, err := orgSvc.CreateProject(ctx, victimOrg.ID, "b3-victim-proj", "B3 Victim Proj", "go")
	if err != nil {
		t.Fatalf("create victim project: %v", err)
	}

	hbCfg, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	victimMonitor, err := s.uptime.Create(ctx, uptime.Monitor{
		ProjectID:         victimProject.ID,
		Name:              "b3 victim monitor",
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

	victimTeam, err := orgSvc.CreateTeam(ctx, victimOrg.ID, "b3-victim-team", "B3 Victim Team")
	if err != nil {
		t.Fatalf("create victim team: %v", err)
	}

	victimSP, err := s.uptime.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: victimProject.ID,
		Title:     "B3 Victim SP",
		Enabled:   true,
	}, nil)
	if err != nil {
		t.Fatalf("create victim statuspage: %v", err)
	}

	issueRes, err := s.h.Issues.Upsert(ctx, victimProject.ID, "b3-victim-fp", "B3 victim issue", "b3.Culprit", "error", "", time.Now())
	if err != nil {
		t.Fatalf("seed victim issue: %v", err)
	}

	perfRes, err := s.h.PerfIssues.Record(ctx, victimProject.ID, trace.Finding{
		Kind:        trace.KindNPlusOne,
		Culprit:     "GET /b3-victim",
		Fingerprint: "b3-victim-perf-fp",
		Description: "SELECT * FROM b3_victims WHERE id = ?",
		Evidence:    map[string]any{"count": 3, "total_ms": int64(30), "span_ids": []string{"s1"}},
	}, "b3-victim-trace")
	if err != nil {
		t.Fatalf("seed victim perf-issue: %v", err)
	}

	v := victimIDs{
		orgID:        victimOrg.ID,
		projectID:    victimProject.ID,
		monitorID:    victimMonitor.ID,
		statuspageID: victimSP.ID,
		teamID:       victimTeam.ID,
		issueID:      issueRes.IssueID,
		perfIssueID:  perfRes.Issue.ID,
	}

	exemptLeave := guards.ExemptedValues(leaveConfirmExemption)
	seenLeave := make(map[string]bool)

	tested := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, path, ok := strings.Cut(route, " ")
		if !ok || !isStrangerScopedRoute(path) {
			continue
		}
		tested++
		concrete := concreteVictimPath(t, path, v)

		var resp *http.Response
		switch method {
		case http.MethodGet:
			resp = getWithCookie(t, s.srv, concrete, strangerCookie)
		case http.MethodPost:
			resp = postForm(t, s.srv, concrete, url.Values{}, s.srv.URL, strangerCookie)
		default:
			t.Fatalf("маршрут %q: неожиданный метод %q — обнови тест", route, method)
			continue
		}
		code := statusOf(t, resp)
		if code != http.StatusNotFound && code != http.StatusForbidden {
			if exemptLeave[route] {
				seenLeave[route] = true
				continue
			}
			t.Errorf("%s %s (чужак, valid-but-foreign id) статус = %d, ожидали 404 или 403 — ГЕЙТ ПРОПУСТИЛ ЧУЖАКА", method, concrete, code)
		}
	}
	if tested == 0 {
		t.Fatal("не найдено ни одного project/org-scoped маршрута — предикат isStrangerScopedRoute сломан?")
	}
	guards.CheckExemptions(t, "TestAuthzBehaviorStrangerRejectedOnScopedRoutes", leaveConfirmExemption, 1, seenLeave)

	leavePath := "/orgs/" + strconv.FormatInt(v.orgID, 10) + "/settings/leave"
	leaveResp := postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	if code := statusOf(t, leaveResp); code != http.StatusNotFound {
		t.Errorf("POST %s confirmed=yes (чужак) статус = %d, ожидали 404 — РЕАЛЬНАЯ МУТАЦИЯ НЕ ЗАЩИЩЕНА", leavePath, code)
	}

	if got, err := orgSvc.GetProject(ctx, victimProject.ID); err != nil || got.Name != victimProject.Name {
		t.Errorf("victim project mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := orgSvc.Get(ctx, victimOrg.ID); err != nil || got.Name != victimOrg.Name {
		t.Errorf("victim org mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.uptime.Get(ctx, victimMonitor.ID); err != nil || !got.Enabled {
		t.Errorf("victim monitor mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if _, err := orgSvc.TeamOrg(ctx, victimTeam.ID); err != nil {
		t.Errorf("victim team deleted by stranger: err=%v", err)
	}
	if got, err := s.uptime.StatusPageByID(ctx, victimSP.ID); err != nil || got.PublicID != victimSP.PublicID || !got.Enabled {
		t.Errorf("victim statuspage mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.h.Issues.Get(ctx, issueRes.IssueID); err != nil || got.Status != "unresolved" {
		t.Errorf("victim issue mutated by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.h.PerfIssues.Get(ctx, victimProject.ID, perfRes.Issue.ID); err != nil || got.Status != "unresolved" {
		t.Errorf("victim perf-issue mutated by stranger: got=%+v err=%v", got, err)
	}
}

func TestAuthzBehaviorMemberRejectedOnAdminRoutes(t *testing.T) {
	s := newUptimeStack(t)
	s.h.Alerts = alert.NewService(s.pool)
	s.h.Outbox = notify.NewOutbox(s.pool)

	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	orgSettingsRegister(t, authSvc, "k14-bootstrap-sink@example.com")

	ownerID, _ := orgSettingsRegister(t, authSvc, "k14-owner@example.com")
	victimOrg, err := orgSvc.CreateOrg(ctx, "k14-org", "K14 Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	victimProject, err := orgSvc.CreateProject(ctx, victimOrg.ID, "k14-proj", "K14 Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	victimTeam, err := orgSvc.CreateTeam(ctx, victimOrg.ID, "k14-team", "K14 Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	v := victimIDs{orgID: victimOrg.ID, projectID: victimProject.ID, teamID: victimTeam.ID}

	type actor struct {
		label  string
		cookie *http.Cookie
	}
	var actors []actor

	memberID, memberCookie := orgSettingsRegister(t, authSvc, "k14-member@example.com")
	if err := orgSvc.AddMember(ctx, victimOrg.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	actors = append(actors, actor{"участник (role=member)", memberCookie})

	operatorID, operatorCookie := orgSettingsRegister(t, authSvc, "k14-operator@example.com")
	if err := orgSvc.AddMember(ctx, victimOrg.ID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, victimTeam.ID, operatorID); err != nil {
		t.Fatalf("add operator to team: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, victimProject.ID, victimTeam.ID); err != nil {
		t.Fatalf("attach team to project: %v", err)
	}
	actors = append(actors, actor{"оператор (team-attached, role=member, canOperateProject=true)", operatorCookie})

	tested := 0
	for _, a := range actors {
		for _, route := range s.h.RegisteredRoutes() {
			method, path, ok := strings.Cut(route, " ")
			if !ok || !isStrangerScopedRoute(path) {
				continue
			}
			lvl, known := routeAuthz[route]
			if !known || (lvl != lvlAdmin && lvl != lvlOwner && lvl != lvlInstanceAdmin) {
				continue
			}
			tested++
			concrete := concreteVictimPath(t, path, v)

			var resp *http.Response
			switch method {
			case http.MethodGet:
				resp = getWithCookie(t, s.srv, concrete, a.cookie)
			case http.MethodPost:
				resp = postForm(t, s.srv, concrete, url.Values{}, s.srv.URL, a.cookie)
			default:
				t.Fatalf("маршрут %q: неожиданный метод %q — обнови тест", route, method)
				continue
			}
			code := statusOf(t, resp)
			if code != http.StatusNotFound && code != http.StatusForbidden {
				t.Errorf("%s %s (%s, своя организация, недостаточная роль) статус = %d, ожидали 404 или 403 — ГЕЙТ ПРОПУСТИЛ УЧАСТНИКА НА ADMIN-МАРШРУТ", method, concrete, a.label, code)
			}
		}
	}
	if tested == 0 {
		t.Fatal("не найдено ни одного admin/owner/instance_admin маршрута с адресацией по id в routeAuthz — фильтр или карта сломаны?")
	}
}

func TestAuthzBehaviorRoleNotSharedAcrossOrgs(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	orgSettingsRegister(t, authSvc, "k14x-bootstrap-sink@example.com")

	dualID, dualCookie := orgSettingsRegister(t, authSvc, "k14x-dual@example.com")

	ownerA, _ := orgSettingsRegister(t, authSvc, "k14x-owner-a@example.com")
	orgA, err := orgSvc.CreateOrg(ctx, "k14x-org-a", "K14x Org A", ownerA)
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	if err := orgSvc.AddMember(ctx, orgA.ID, dualID, org.RoleAdmin); err != nil {
		t.Fatalf("add dual as admin of A: %v", err)
	}
	projA, err := orgSvc.CreateProject(ctx, orgA.ID, "k14x-proj-a", "K14x Proj A", "go")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}

	ownerB, _ := orgSettingsRegister(t, authSvc, "k14x-owner-b@example.com")
	orgB, err := orgSvc.CreateOrg(ctx, "k14x-org-b", "K14x Org B", ownerB)
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	if err := orgSvc.AddMember(ctx, orgB.ID, dualID, org.RoleMember); err != nil {
		t.Fatalf("add dual as member of B: %v", err)
	}
	projB, err := orgSvc.CreateProject(ctx, orgB.ID, "k14x-proj-b", "K14x Proj B", "go")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}
	teamB, err := orgSvc.CreateTeam(ctx, orgB.ID, "k14x-team-b", "K14x Team B")
	if err != nil {
		t.Fatalf("create team B: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, teamB.ID, dualID); err != nil {
		t.Fatalf("attach dual to team B: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, projB.ID, teamB.ID); err != nil {
		t.Fatalf("attach team B to project B: %v", err)
	}

	if role, rerr := orgSvc.Role(ctx, orgA.ID, dualID); rerr != nil || role != org.RoleAdmin {
		t.Fatalf("Role(orgA, dual) = %v, %v, want RoleAdmin, nil", role, rerr)
	}
	if role, rerr := orgSvc.Role(ctx, orgB.ID, dualID); rerr != nil || role != org.RoleMember {
		t.Fatalf("Role(orgB, dual) = %v, %v, want RoleMember, nil", role, rerr)
	}

	issuesBPath := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/issues"
	if code := statusOf(t, getWithCookie(t, s.srv, issuesBPath, dualCookie)); code != http.StatusOK {
		t.Errorf("GET %s (dual, легитимный team-доступ в B) = %d, want 200", issuesBPath, code)
	}

	issueB, err := s.h.Issues.Upsert(ctx, projB.ID, "k14x-fp", "K14x issue", "k14x.Culprit", "error", "", time.Now())
	if err != nil {
		t.Fatalf("seed issue B: %v", err)
	}
	issueStatusPath := "/issues/" + strconv.FormatInt(issueB.IssueID, 10) + "/status"
	statusResp := postForm(t, s.srv, issueStatusPath, url.Values{"status": {"resolved"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, statusResp); code != http.StatusSeeOther {
		t.Errorf("POST %s (dual, легитимный team-доступ в B) = %d, want 303", issueStatusPath, code)
	}

	renamePathA := "/projects/" + strconv.FormatInt(projA.ID, 10) + "/settings/rename"
	renameA := postForm(t, s.srv, renamePathA, url.Values{"name": {"K14x Proj A Renamed"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, renameA); code != http.StatusSeeOther {
		t.Fatalf("POST %s (dual, реальный admin организации A) = %d, want 303 — контроль сломан, дальнейший результат недостоверен", renamePathA, code)
	}

	renamePathB := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/settings/rename"
	renameB := postForm(t, s.srv, renamePathB, url.Values{"name": {"k14x-hijacked"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, renameB); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Errorf("POST %s (dual, admin ЧУЖОЙ организации A) статус = %d, ожидали 404 или 403 — РОЛЬ ИЗ ОРГАНИЗАЦИИ A ПРОТЕКЛА В B", renamePathB, code)
	}
	if got, gerr := orgSvc.GetProject(ctx, projB.ID); gerr != nil || got.Name != projB.Name {
		t.Errorf("project B переименован admin'ом чужой организации: got=%+v err=%v", got, gerr)
	}
}
