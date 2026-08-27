package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// logsLinkStartRe/logsLinkEndRe — проверяют, что ссылка «Логи вокруг события»
// несёт окно start=/end= (unix-секунды). url.Values.Encode() сортирует
// параметры по имени (end= раньше start=), поэтому порядок не фиксируется —
// оба паттерна проверяются независимо.
var (
	logsLinkStartRe = regexp.MustCompile(`/logs\?[^"]*start=\d+`)
	logsLinkEndRe   = regexp.MustCompile(`/logs\?[^"]*end=\d+`)
)

func hasLogsLinkWindow(body string) bool {
	return logsLinkStartRe.MatchString(body) && logsLinkEndRe.MatchString(body)
}

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

	// Произвольный диапазон графика частоты (?period=custom&start&end):
	// handler parseTimeRange/autoStep + селектор в режиме custom, событие
	// сохраняется как скрытое поле формы.
	custQ := issuePath + "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, custQ, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", custQ, resp.StatusCode, cbody)
	}
	if !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("GET %s did not render custom range selected: %s", custQ, cbody)
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

// TestWebIssueDetailCopyToolbar — тулбар «Скопировать для ИИ» (Task 3):
// при выбранном событии страница несёт обе кнопки (data-copy-format="md"/
// "txt") и два скрытых textarea-блоба (data-copy-target); issue без единого
// события — этих узлов нет вовсе (copyMD/copyTXT пусты, selected == nil, см.
// issuedetail.go).
func TestWebIssueDetailCopyToolbar(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, ownerCookie := registerAndLogin(t, s, "issuedetail-copy-owner@example.com")
	project := createProject(t, s, ownerID, "issuedetail-copy-org", "issuedetail-copy-proj")
	now := time.Now().UTC()

	// Issue с ≥1 событием → тулбар присутствует.
	withEvent, err := s.issues.Upsert(context.Background(), project.ID, "fp-copy-with-event", "NullPointerException", "pkg/a.go:10", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}
	evID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:             evID,
		ProjectID:      project.ID,
		IssueID:        withEvent.IssueID,
		Timestamp:      now,
		Level:          "error",
		Message:        "boom",
		ExceptionType:  "NullPointerException",
		ExceptionValue: "boom",
	})
	s.flushEvents(t)

	withEventPath := "/issues/" + strconv.FormatInt(withEvent.IssueID, 10)
	resp := getWithCookie(t, s.srv, withEventPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", withEventPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `data-copy-format="md"`) {
		t.Fatalf("GET %s missing data-copy-format=\"md\": %s", withEventPath, body)
	}
	if !strings.Contains(string(body), `data-copy-format="txt"`) {
		t.Fatalf("GET %s missing data-copy-format=\"txt\": %s", withEventPath, body)
	}
	if strings.Count(string(body), "<textarea") < 2 {
		t.Fatalf("GET %s expected 2+ <textarea (copy blobs): %s", withEventPath, body)
	}
	if !strings.Contains(string(body), `data-copy-target="copy-md"`) || !strings.Contains(string(body), `data-copy-target="copy-txt"`) {
		t.Fatalf("GET %s missing data-copy-target attributes: %s", withEventPath, body)
	}

	// Issue без единого события → selected == nil, тулбара нет.
	noEvents, err := s.issues.Upsert(context.Background(), project.ID, "fp-copy-no-events", "OtherError", "pkg/b.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue (no events): %v", err)
	}
	noEventsPath := "/issues/" + strconv.FormatInt(noEvents.IssueID, 10)
	resp = getWithCookie(t, s.srv, noEventsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", noEventsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "data-copy-format") {
		t.Fatalf("GET %s (no events) unexpectedly has copy toolbar: %s", noEventsPath, body)
	}
	if strings.Contains(string(body), "data-copy-target") {
		t.Fatalf("GET %s (no events) unexpectedly has copy blob targets: %s", noEventsPath, body)
	}
}

// TestWebIssueDetailLogsLink — «Логи вокруг события» (C3 задача 3): ссылка
// присутствует безусловно (в отличие от trace-ссылки, которая требует
// hasTrace) и всегда несёт окно ±5м, иначе логи события старше суток
// отсекались бы дефолтным 24ч-окном /logs. С trace_id — окно + trace_id;
// без trace_id — окно + environment (без trace_id вовсе).
func TestWebIssueDetailLogsLink(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, ownerCookie := registerAndLogin(t, s, "issuedetail-logs-owner@example.com")
	project := createProject(t, s, ownerID, "issuedetail-logs-org", "issuedetail-logs-proj")
	now := time.Now().UTC()

	// Событие С trace_id → ссылка несёт trace_id И окно start=/end=.
	withTrace, err := s.issues.Upsert(context.Background(), project.ID, "fp-logs-trace", "NullPointerException", "pkg/a.go:10", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}
	evTraceID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:            evTraceID,
		ProjectID:     project.ID,
		IssueID:       withTrace.IssueID,
		Timestamp:     now,
		Level:         "error",
		Message:       "boom",
		ExceptionType: "NullPointerException",
		TraceID:       "tr-123",
		Environment:   "production",
	})
	s.flushEvents(t)

	withTracePath := "/issues/" + strconv.FormatInt(withTrace.IssueID, 10)
	resp := getWithCookie(t, s.srv, withTracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", withTracePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "trace_id=tr-123") {
		t.Fatalf("GET %s missing trace_id= in logs link: %s", withTracePath, body)
	}
	if !hasLogsLinkWindow(string(body)) {
		t.Fatalf("GET %s logs link missing start=/end= window: %s", withTracePath, body)
	}

	// Событие БЕЗ trace_id, с environment=prod → окно + environment, без trace_id.
	noTrace, err := s.issues.Upsert(context.Background(), project.ID, "fp-logs-no-trace", "OtherError", "pkg/b.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue (no trace): %v", err)
	}
	evNoTraceID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:            evNoTraceID,
		ProjectID:     project.ID,
		IssueID:       noTrace.IssueID,
		Timestamp:     now,
		Level:         "error",
		Message:       "boom",
		ExceptionType: "OtherError",
		Environment:   "prod",
	})
	s.flushEvents(t)

	noTracePath := "/issues/" + strconv.FormatInt(noTrace.IssueID, 10)
	resp = getWithCookie(t, s.srv, noTracePath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", noTracePath, resp.StatusCode, body)
	}
	if !hasLogsLinkWindow(string(body)) {
		t.Fatalf("GET %s logs link missing start=/end= window: %s", noTracePath, body)
	}
	if !strings.Contains(string(body), "environment=prod") {
		t.Fatalf("GET %s missing environment=prod in logs link: %s", noTracePath, body)
	}
	if strings.Contains(string(body), "trace_id=") {
		t.Fatalf("GET %s (no trace) unexpectedly has trace_id= in logs link: %s", noTracePath, body)
	}
}

// TestWebIssueDetailExportButtonShowsPIIOnlyForOwner — та же боевая
// проводка, что TestWebIssuesListExportButtonsShowPIIOnlyForOwner
// (issues_test.go), только для третьей точки входа — карточки issue
// (issuedetail.go). До находки аудита P2-UX-3 галки include_pii здесь не
// было вовсе: владелец не мог выгрузить события ОДНОЙ issue без маски,
// только весь проект целиком с генерической формы страницы «Выгрузки».
// Владелец (CanManage) видит галку, оператор без CanManage (доступ только
// через команду) — нет; scope_issue_id и выбор формата остаются у обоих.
func TestWebIssueDetailExportButtonShowsPIIOnlyForOwner(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issuedetail-pii-owner@example.com")
	project := createProject(t, s, ownerID, "issuedetail-pii-org", "issuedetail-pii-proj")

	operatorID, operatorCookie := registerAndLogin(t, s, "issuedetail-pii-operator@example.com")
	if err := s.org.AddMember(context.Background(), project.OrgID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	team, err := s.org.CreateTeam(context.Background(), project.OrgID, "issuedetail-pii-team", "issuedetail-pii-team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(context.Background(), team.ID, operatorID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := s.org.AttachTeam(context.Background(), project.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	s.h.Exports = export.NewStore(s.pool)
	t.Cleanup(func() { s.h.Exports = nil })

	now := time.Now().UTC()
	r, err := s.issues.Upsert(context.Background(), project.ID, "fp-pii-detail", "NullPointerException", "pkg/a.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}
	s.batcher.Add(event.Event{
		ID:        uuid.NewString(),
		ProjectID: project.ID,
		IssueID:   r.IssueID,
		Timestamp: now,
		Level:     "error",
		Message:   "boom",
		Tags:      map[string]string{},
	})
	s.flushEvents(t)

	issuePath := "/issues/" + strconv.FormatInt(r.IssueID, 10)

	ownerBody := readAll(t, getWithCookie(t, s.srv, issuePath, ownerCookie))
	if !strings.Contains(ownerBody, `name="include_pii"`) {
		t.Errorf("владельцу не показана галка include_pii на карточке issue: %s", ownerBody)
	}
	if !strings.Contains(ownerBody, `<select name="format"`) {
		t.Error("владельцу не показан выбор формата на карточке issue")
	}
	if !strings.Contains(ownerBody, `name="scope_issue_id" value="`+strconv.FormatInt(r.IssueID, 10)+`"`) {
		t.Error("scope_issue_id потерян на карточке issue")
	}

	operatorBody := readAll(t, getWithCookie(t, s.srv, issuePath, operatorCookie))
	if strings.Contains(operatorBody, `name="include_pii"`) {
		t.Error("оператору без CanManage показана галка include_pii на карточке issue")
	}
	if !strings.Contains(operatorBody, `<select name="format"`) {
		t.Error("оператору должен остаться выбор формата на карточке issue")
	}
}
