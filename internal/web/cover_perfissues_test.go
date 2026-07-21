package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestCoverPerfIssueSetStatusBranches — недокрытые ветки perfIssueSetStatus:
// невалидный {id} → 404; несуществующая (found=false) проблема → 404; неизвестный
// статус → 422; валидный статус → 303.
func TestCoverPerfIssueSetStatusBranches(t *testing.T) {
	s := newPerfIssuesStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "cover-pi-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "cover-pi-co", "Cover PI", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(ctx, o.ID, "cover-pi-proj", "Cover PI Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	id := s.insertPerfIssue(t, proj.ID, 5, trace.KindNPlusOne, "fp-cover-pi",
		"N+1", "GET /x", "unresolved", "trace-x", `{"count":5}`)

	// Невалидный {id} → 404.
	resp := postForm(t, s.srv, "/perf-issues/not-a-number/status", url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST perf-issue status (bad id) = %d, want 404", resp.StatusCode)
	}

	// Несуществующая проблема (found=false) → 404.
	resp = postForm(t, s.srv, "/perf-issues/9999999/status", url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST perf-issue status (nonexistent) = %d, want 404", resp.StatusCode)
	}

	statusPath := "/perf-issues/" + strconv.FormatInt(id, 10) + "/status"

	// Неизвестный статус → 422.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"bogus"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST perf-issue status (invalid) = %d, want 422", resp.StatusCode)
	}

	// Валидный статус → 303.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST perf-issue status (valid) = %d, want 303", resp.StatusCode)
	}
	if got := s.statusOf(t, id); got != "resolved" {
		t.Fatalf("status after resolve = %q, want resolved", got)
	}
}
