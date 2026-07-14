package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
)

// issueStacktrace — JSON исключения (формат из брифа задачи 7) с двумя
// фреймами: один in_app, один системный. Используется для проверки, что
// парсер стектрейса в issuedetail.go извлекает и отображает оба.
const issueStacktrace = `{"values":[{"type":"NullPointerException","value":"boom","stacktrace":{"frames":[` +
	`{"function":"main.inner","module":"main","filename":"main.go","lineno":42,"in_app":true},` +
	`{"function":"runtime.goexit","module":"runtime","filename":"runtime.go","lineno":1,"in_app":false}` +
	`]}}]}`

func TestWebIssueDetail(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issuedetail-owner@example.com")
	project := createProject(t, s, ownerID, "issuedetail-org", "issuedetail-proj")

	now := time.Now().UTC()
	r1, err := s.issues.Upsert(context.Background(), project.ID, "fp-detail", "NullPointerException", "pkg/a.go:10", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	ev1ID := uuid.NewString()
	ev2ID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:        ev1ID,
		ProjectID: project.ID,
		IssueID:   r1.IssueID,
		Timestamp: now.Add(-2 * time.Hour),
		Level:     "error",
		Message:   "plain boom",
		Tags:      map[string]string{},
	})
	s.batcher.Add(event.Event{
		ID:             ev2ID,
		ProjectID:      project.ID,
		IssueID:        r1.IssueID,
		Timestamp:      now.Add(-1 * time.Hour),
		Level:          "error",
		Message:        "boom",
		ExceptionType:  "NullPointerException",
		ExceptionValue: "boom",
		Stacktrace:     issueStacktrace,
		Environment:    "production",
		Release:        "1.2.3",
		Tags:           map[string]string{"foo": "bar"},
	})
	s.flushEvents(t)

	issuePath := "/issues/" + strconv.FormatInt(r1.IssueID, 10)

	// GET issue detail → 200: title, кнопка Resolve, <svg (график), оба event id.
	resp := getWithCookie(t, s.srv, issuePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s missing title: %s", issuePath, body)
	}
	if !strings.Contains(string(body), `name="status" value="resolved"`) {
		t.Fatalf("GET %s missing Resolve button: %s", issuePath, body)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s missing <svg chart: %s", issuePath, body)
	}
	if !strings.Contains(string(body), ev1ID) || !strings.Contains(string(body), ev2ID) {
		t.Fatalf("GET %s missing event ids %s / %s: %s", issuePath, ev1ID, ev2ID, body)
	}
	// Breadcrumb back to the issue's own project (fix 2: issue detail had no
	// way back to the issues list of a non-first project).
	backToIssues := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	if !strings.Contains(string(body), backToIssues) {
		t.Fatalf("GET %s missing breadcrumb link %q: %s", issuePath, backToIssues, body)
	}

	// ?event=ev2ID → фреймы: function присутствует, in-app класс есть.
	resp = getWithCookie(t, s.srv, issuePath+"?event="+ev2ID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=%s status = %d, want 200: %s", issuePath, ev2ID, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "main.inner") {
		t.Fatalf("GET %s?event=%s missing frame function: %s", issuePath, ev2ID, body)
	}
	if !strings.Contains(string(body), "runtime.goexit") {
		t.Fatalf("GET %s?event=%s missing system frame function: %s", issuePath, ev2ID, body)
	}
	if !strings.Contains(string(body), "in-app") {
		t.Fatalf("GET %s?event=%s missing in-app class: %s", issuePath, ev2ID, body)
	}

	// POST status=resolved → 303, статус в PG resolved, страница теперь
	// показывает Unresolve вместо Resolve.
	statusPath := issuePath + "/status"
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", statusPath, resp.StatusCode)
	}
	got, err := s.issues.Get(context.Background(), r1.IssueID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if got.Status != "resolved" {
		t.Fatalf("issue status = %q, want resolved", got.Status)
	}
	resp = getWithCookie(t, s.srv, issuePath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="status" value="unresolved"`) {
		t.Fatalf("GET %s after resolve missing Unresolve button: %s", issuePath, body)
	}
	if strings.Contains(string(body), `name="status" value="resolved"`) {
		t.Fatalf("GET %s after resolve still shows Resolve button: %s", issuePath, body)
	}

	// POST assign=ownerID (участник организации) → 303, assignee_id проставлен.
	assignPath := issuePath + "/assign"
	resp = postForm(t, s.srv, assignPath, url.Values{"assignee": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", assignPath, resp.StatusCode)
	}
	got, err = s.issues.Get(context.Background(), r1.IssueID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if got.AssigneeID == nil || *got.AssigneeID != ownerID {
		t.Fatalf("issue assignee = %v, want %d", got.AssigneeID, ownerID)
	}

	// POST assign=постороннему юзеру (не участник организации проекта) → 422.
	strangerID, _ := registerAndLogin(t, s, "issuedetail-stranger@example.com")
	resp = postForm(t, s.srv, assignPath, url.Values{"assignee": {strconv.FormatInt(strangerID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (non-member assignee) status = %d, want 422", assignPath, resp.StatusCode)
	}

	// POST status без same-origin Origin/Referer → 403.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"unresolved"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", statusPath, resp.StatusCode)
	}

	// Доступ чужим юзером (не участник организации) → 404.
	_, outsiderCookie := registerAndLogin(t, s, "issuedetail-outsider@example.com")
	resp = getWithCookie(t, s.srv, issuePath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", issuePath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (outsider) status = %d, want 404", statusPath, resp.StatusCode)
	}

	// Несуществующий issue → 404.
	resp = getWithCookie(t, s.srv, "/issues/999999999", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /issues/999999999 status = %d, want 404", resp.StatusCode)
	}

	// Malformed event ID (не UUID) → 200 (graceful degradation, page without event details).
	resp = getWithCookie(t, s.srv, issuePath+"?event=<script>garbage", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=<script>garbage status = %d, want 200: %s", issuePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s?event=<script>garbage missing title: %s", issuePath, body)
	}
}
