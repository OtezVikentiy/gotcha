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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// maintenanceStack — own stand, PG-only: maintenance-window routes never
// touch ClickHouse (no UptimeQuery involved), so unlike monitorFormStack this
// one skips the CH container entirely for a faster test run.
type maintenanceStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
}

func newMaintenanceStack(t *testing.T) *maintenanceStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query
	uptimeSvc := uptime.NewService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.Register(mux)

	return &maintenanceStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc}
}

func maintenanceOwnerAndMember(t *testing.T, s *maintenanceStack, namePrefix string) (org.Project, *http.Cookie, *http.Cookie) {
	t.Helper()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, namePrefix+"-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, namePrefix+"-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), namePrefix+"-co", namePrefix+" Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, namePrefix+"-proj", namePrefix+" Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, namePrefix+"-team")
	return proj, ownerCookie, memberCookie
}

// TestWebMaintenanceCreateOneOff — форма разового окна: datetime-local +
// выбранный из фиксированного списка TZ дают верный Window в БД.
func TestWebMaintenanceCreateOneOff(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintoneoff")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"name":      {"DB upgrade"},
		"kind":      {"oneoff"},
		"starts_at": {"2026-08-01T02:00"},
		"ends_at":   {"2026-08-01T04:00"},
		"timezone":  {"Europe/Moscow"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303: %s", path, resp.StatusCode, body)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("len(windows) = %d, want 1", len(windows))
	}
	w := windows[0]
	if w.Name != "DB upgrade" || w.Weekly {
		t.Fatalf("window = %+v, want name=DB upgrade weekly=false", w)
	}
	if w.Timezone != "Europe/Moscow" {
		t.Fatalf("Timezone = %q, want Europe/Moscow", w.Timezone)
	}
	if w.StartsAt == nil || w.EndsAt == nil {
		t.Fatalf("StartsAt/EndsAt = %v/%v, want both set", w.StartsAt, w.EndsAt)
	}
	// 2026-08-01 02:00 MSK (UTC+3) == 2026-07-31 23:00 UTC.
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	want := time.Date(2026, 8, 1, 2, 0, 0, 0, loc)
	if !w.StartsAt.Equal(want) {
		t.Fatalf("StartsAt = %v, want %v", w.StartsAt, want)
	}

	// GET the page back -> shows the created window.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "DB upgrade") {
		t.Fatalf("GET %s missing window name: %s", path, body)
	}
}

// TestWebMaintenanceCreateWeekly — форма еженедельного окна: день недели +
// HH:MM + TZ.
func TestWebMaintenanceCreateWeekly(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintweekly")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"name":       {"Weekly backup window"},
		"kind":       {"weekly"},
		"weekday":    {"2"}, // вторник
		"start_time": {"01:30"},
		"end_time":   {"02:30"},
		"timezone":   {"UTC"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303: %s", path, resp.StatusCode, body)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("len(windows) = %d, want 1", len(windows))
	}
	w := windows[0]
	if !w.Weekly || w.Weekday != 2 || w.StartTime != "01:30" || w.EndTime != "02:30" || w.Timezone != "UTC" {
		t.Fatalf("window = %+v, want weekly tuesday 01:30-02:30 UTC", w)
	}
}

// TestWebMaintenanceCreateCustomTimezone — выбор "Другой" (пустое значение
// select) переключает на свободный IANA-текст.
func TestWebMaintenanceCreateCustomTimezone(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "mainttz")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"name":            {"Custom TZ window"},
		"kind":            {"weekly"},
		"weekday":         {"0"},
		"start_time":      {"10:00"},
		"end_time":        {"11:00"},
		"timezone":        {""},
		"timezone_custom": {"America/New_York"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", path, resp.StatusCode)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 1 || windows[0].Timezone != "America/New_York" {
		t.Fatalf("windows = %+v, want single window with tz America/New_York", windows)
	}
}

// TestWebMaintenanceInvalidWindowShows422 — пустое имя -> ErrInvalidWindow ->
// 422, ничего не создаётся.
func TestWebMaintenanceInvalidWindowShows422(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintinvalid")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"name":      {""}, // window.Name required by validateWindow
		"starts_at": {"2026-08-01T02:00"},
		"ends_at":   {"2026-08-01T04:00"},
		"timezone":  {"UTC"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s status = %d, want 422: %s", path, resp.StatusCode, body)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("len(windows) = %d, want 0 (invalid create must not persist)", len(windows))
	}
}

// TestWebMaintenanceDelete — creates then deletes a window; it's gone from
// Windows afterwards.
func TestWebMaintenanceDelete(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintdelete")

	win, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: proj.ID, Name: "To delete", Weekly: true, Weekday: 1,
		StartTime: "00:00", EndTime: "01:00", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create window: %v", err)
	}

	deletePath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance/delete"
	resp := postForm(t, s.srv, deletePath, url.Values{"window_id": {strconv.FormatInt(win.ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303: %s", deletePath, resp.StatusCode, body)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("len(windows) = %d, want 0 after delete", len(windows))
	}
}

// TestWebMaintenanceDeleteForeignProject404 — a window_id belonging to
// another project must 404, not silently delete it.
func TestWebMaintenanceDeleteForeignProject404(t *testing.T) {
	s := newMaintenanceStack(t)
	projA, ownerCookieA, _ := maintenanceOwnerAndMember(t, s, "maintforeigna")
	projB, ownerCookieB, _ := maintenanceOwnerAndMember(t, s, "maintforeignb")
	_ = ownerCookieB

	winA, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: projA.ID, Name: "Project A window", Weekly: true, Weekday: 3,
		StartTime: "00:00", EndTime: "01:00", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create window: %v", err)
	}

	// Try to delete A's window through B's maintenance/delete path.
	deletePathB := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/maintenance/delete"
	resp := postForm(t, s.srv, deletePathB, url.Values{"window_id": {strconv.FormatInt(winA.ID, 10)}}, s.srv.URL, ownerCookieB)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (foreign window) status = %d, want 404", deletePathB, resp.StatusCode)
	}

	windows, err := s.uptime.Windows(context.Background(), projA.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("len(windows) = %d, want 1 (must survive foreign delete attempt)", len(windows))
	}
	_ = ownerCookieA
}

// TestWebMaintenanceMemberForbidden — member (view access, not owner/admin)
// gets 404 on GET and both POST routes.
func TestWebMaintenanceMemberForbidden(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, memberCookie := maintenanceOwnerAndMember(t, s, "maintforbid")

	win, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: proj.ID, Name: "Existing", Weekly: true, Weekday: 1,
		StartTime: "00:00", EndTime: "01:00", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create window: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	deletePath := path + "/delete"

	resp := getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", path, resp.StatusCode)
	}

	resp = postForm(t, s.srv, path, url.Values{"name": {"x"}, "starts_at": {"2026-08-01T02:00"}, "ends_at": {"2026-08-01T03:00"}, "timezone": {"UTC"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", path, resp.StatusCode)
	}

	resp = postForm(t, s.srv, deletePath, url.Values{"window_id": {strconv.FormatInt(win.ID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}

	// Sanity: owner still works.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200", path, resp.StatusCode)
	}
}

// TestWebMaintenanceKindIsExclusive — тип окна взаимоисключающий: при
// kind=oneoff еженедельные поля в запросе игнорируются (форма их и не
// показывает, но скрытые поля всё равно уходят в POST). Без этого
// заполненные «на всякий случай» день недели и время могли бы просочиться в
// разовое окно.
func TestWebMaintenanceKindIsExclusive(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintexcl")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"name":       {"One-off with stray weekly fields"},
		"kind":       {"oneoff"},
		"starts_at":  {"2026-08-01T02:00"},
		"ends_at":    {"2026-08-01T04:00"},
		"timezone":   {"UTC"},
		// поля еженедельной ветки, оставшиеся от переключения режима:
		"weekday":    {"3"},
		"start_time": {"05:00"},
		"end_time":   {"06:00"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", path, resp.StatusCode)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	w := windows[0]
	if w.Weekly || w.Weekday != 0 || w.StartTime != "" || w.EndTime != "" {
		t.Fatalf("еженедельные поля просочились в разовое окно: %+v", w)
	}
}
