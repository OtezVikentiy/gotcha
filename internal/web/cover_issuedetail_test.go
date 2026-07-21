package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverIssueDetailBranches — недокрытые ветки issue-detail: невалидный/
// несуществующий id → 404, чужой проект → 404, ?event с битым и неизвестным
// UUID (graceful), смена статуса (403/404/422/303), назначение (403/422/303).
func TestCoverIssueDetailBranches(t *testing.T) {
	s := newIssuesStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := registerAndLogin(t, s, "cover-id-owner@example.com")
	project := createProject(t, s, ownerID, "cover-id-org", "cover-id-proj")
	orgID, err := s.org.ProjectOrg(ctx, project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}
	memberID, _ := registerAndLogin(t, s, "cover-id-member@example.com")
	if err := s.org.AddMember(ctx, orgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	_, outsiderCookie := registerAndLogin(t, s, "cover-id-outsider@example.com")

	now := time.Now().UTC()
	r1, err := s.issues.Upsert(ctx, project.ID, "fp-cover-id", "Boom", "pkg/a.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	issuePath := "/issues/" + strconv.FormatInt(r1.IssueID, 10)

	// Невалидный id → 404.
	resp := getWithCookie(t, s.srv, "/issues/not-a-number", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /issues/not-a-number status = %d, want 404", resp.StatusCode)
	}

	// Несуществующий (числовой) id → 404.
	resp = getWithCookie(t, s.srv, "/issues/9999999", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /issues/9999999 status = %d, want 404", resp.StatusCode)
	}

	// Чужой проект (outsider) → 404.
	resp = getWithCookie(t, s.srv, issuePath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET issue (outsider) status = %d, want 404", resp.StatusCode)
	}

	// ?event с невалидным UUID → 200 (деградация без выбора события).
	resp = getWithCookie(t, s.srv, issuePath+"?event=not-a-uuid", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET issue?event=not-a-uuid status = %d, want 200", resp.StatusCode)
	}

	// ?event с валидным, но неизвестным UUID → 200 (found=false).
	resp = getWithCookie(t, s.srv, issuePath+"?event="+uuid.NewString(), ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET issue?event=<unknown> status = %d, want 200", resp.StatusCode)
	}

	// --- issueSetStatus ---
	// Без Origin → 403.
	resp = postForm(t, s.srv, issuePath+"/status", url.Values{"status": {"resolved"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST status (no origin) status = %d, want 403", resp.StatusCode)
	}
	// Несуществующий id → 404.
	resp = postForm(t, s.srv, "/issues/9999999/status", url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST status (nonexistent) status = %d, want 404", resp.StatusCode)
	}
	// Невалидный статус → 422.
	resp = postForm(t, s.srv, issuePath+"/status", url.Values{"status": {"bogus"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST status (invalid) status = %d, want 422", resp.StatusCode)
	}
	// Валидный статус → 303.
	resp = postForm(t, s.srv, issuePath+"/status", url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST status (valid) status = %d, want 303", resp.StatusCode)
	}

	// --- issueAssign ---
	// Без Origin → 403.
	resp = postForm(t, s.srv, issuePath+"/assign", url.Values{"assignee": {""}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST assign (no origin) status = %d, want 403", resp.StatusCode)
	}
	// Нечисловой assignee → 422.
	resp = postForm(t, s.srv, issuePath+"/assign", url.Values{"assignee": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST assign (bad assignee) status = %d, want 422", resp.StatusCode)
	}
	// Не участник организации → 422.
	_, _ = outsiderCookie, memberID
	outsiderID, _ := registerAndLogin(t, s, "cover-id-nonmember@example.com")
	resp = postForm(t, s.srv, issuePath+"/assign", url.Values{"assignee": {strconv.FormatInt(outsiderID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST assign (non-member) status = %d, want 422", resp.StatusCode)
	}
	// Валидный участник → 303.
	resp = postForm(t, s.srv, issuePath+"/assign", url.Values{"assignee": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST assign (member) status = %d, want 303", resp.StatusCode)
	}
	// Снятие назначения → 303.
	resp = postForm(t, s.srv, issuePath+"/assign", url.Values{"assignee": {""}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST assign (unassign) status = %d, want 303", resp.StatusCode)
	}
}
