package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverTeamsSameOriginAndMember — remove/attach/detach без Origin → 403 и
// под member → 404 (requireTeamRole).
func TestCoverTeamsSameOriginAndMember(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, _ := orgSettingsRegister(t, authSvc, "cover-t2-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "cover-t2-member@example.com")
	o, err := orgSvc.CreateOrg(ctx, "cover-t2-co", "Cover T2", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	team, err := orgSvc.CreateTeam(ctx, o.ID, "cover-t2-team", "Cover T2 Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	tb := "/teams/" + strconv.FormatInt(team.ID, 10)

	for _, sub := range []string{"/members/remove", "/projects", "/projects/detach"} {
		// без Origin → 403.
		resp := postForm(t, s.srv, tb+sub, url.Values{"user_id": {"1"}, "project_id": {"1"}}, "", memberCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (no origin) = %d, want 403", sub, resp.StatusCode)
		}
		// member (не owner/admin) → 404.
		resp = postForm(t, s.srv, tb+sub, url.Values{"user_id": {"1"}, "project_id": {"1"}}, s.srv.URL, memberCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s (member) = %d, want 404", sub, resp.StatusCode)
		}
	}
}

// TestCoverIssuesBulkBranches — issuesBulk: невалидный {id} → 404; неизвестный
// action → 400; пустой список ids → 303 (SetStatusBulk не вызывается); список с
// сортировкой/страницей рендерится.
func TestCoverIssuesBulkBranches(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, ownerCookie := registerAndLogin(t, s, "cover-bulk-owner@example.com")
	project := createProject(t, s, ownerID, "cover-bulk-org", "cover-bulk-proj")

	now := time.Now().UTC()
	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-bulk", "Boom", "pkg/a.go:1", "error", "", now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	bulkPath := issuesPath + "/bulk"

	// Невалидный {id} → 404.
	resp := postForm(t, s.srv, "/projects/not-a-number/issues/bulk", url.Values{"action": {"resolve"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST bulk (bad id) = %d, want 404", resp.StatusCode)
	}

	// Неизвестный action → 400.
	resp = postForm(t, s.srv, bulkPath, url.Values{"action": {"bogus"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST bulk (bad action) = %d, want 400", resp.StatusCode)
	}

	// Пустой ids → 303 (SetStatusBulk пропускается).
	resp = postForm(t, s.srv, bulkPath, url.Values{"action": {"resolve"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST bulk (empty ids) = %d, want 303", resp.StatusCode)
	}

	// Список с сортировкой и страницей — покрывает parsePage/сорт-ветки.
	for _, q := range []string{"?sort=freq", "?sort=first_seen", "?page=2", "?page=abc"} {
		resp = getWithCookie(t, s.srv, issuesPath+q, ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET issues%s = %d, want 200", q, resp.StatusCode)
		}
	}
}
