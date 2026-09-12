package web_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
)

type fakePurger struct {
	mu         sync.Mutex
	projects   []int64
	subjects   []purgeSubjectCall
	exports    []purgeSubjectCall
	subjectErr error // если задан — PurgeSubject возвращает его
	// нулевое значение поля — валидный результат «строк не нашлось», а не «не задано»
	subjectResult telemetry.PurgeResult
}

type purgeSubjectCall struct {
	projectID int64
	sub       telemetry.Subject
}

func (f *fakePurger) PurgeProject(_ context.Context, projectID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = append(f.projects, projectID)
	return nil
}

func (f *fakePurger) PurgeSubject(_ context.Context, projectID int64, sub telemetry.Subject) (telemetry.PurgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, purgeSubjectCall{projectID: projectID, sub: sub})
	return f.subjectResult, f.subjectErr
}

func (f *fakePurger) ExportSubject(_ context.Context, projectID int64, sub telemetry.Subject) (telemetry.SubjectExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exports = append(f.exports, purgeSubjectCall{projectID: projectID, sub: sub})
	return telemetry.SubjectExport{}, nil
}

func TestWebDeleteProject(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	fp := &fakePurger{}
	s.h.Purger = fp

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "delproj-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "delproj-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "delproj-co", "DelProj Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "delproj-proj", "DelProj Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	deletePath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings/delete"

	resp := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", deletePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}
	if len(fp.projects) != 0 {
		t.Fatalf("PurgeProject called on member-denied request: %v", fp.projects)
	}

	// Двухшаговый confirm: CSP default-src 'self' без unsafe-inline не исполнит inline onsubmit="confirm()".
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (owner, unconfirmed) status = %d, want 200: %s", deletePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (owner, unconfirmed) missing confirm page hidden field: %s", deletePath, body)
	}
	if !strings.Contains(string(body), "«DelProj Proj»") {
		t.Fatalf("POST %s (owner, unconfirmed) confirm page does not name the project: %s", deletePath, body)
	}
	if projects, err := orgSvc.ProjectsOf(context.Background(), o.ID); err != nil {
		t.Fatalf("ProjectsOf: %v", err)
	} else {
		found := false
		for _, p := range projects {
			if p.ID == proj.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("project %d removed from PG by unconfirmed POST", proj.ID)
		}
	}
	if len(fp.projects) != 0 {
		t.Fatalf("PurgeProject called on unconfirmed request: %v", fp.projects)
	}

	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", deletePath, resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/projects" {
		t.Fatalf("POST %s (owner) Location = %q, want /projects", deletePath, loc)
	}
	projects, err := orgSvc.ProjectsOf(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("ProjectsOf: %v", err)
	}
	for _, p := range projects {
		if p.ID == proj.ID {
			t.Fatalf("project %d still present in PG after delete", proj.ID)
		}
	}
	if len(fp.projects) != 0 {
		t.Fatalf("PurgeProject вызван из HTTP-запроса: %v — очистка обязана идти фоновым исполнителем", fp.projects)
	}
	var queued bool
	if err := s.pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)",
		proj.ID).Scan(&queued); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if !queued {
		t.Fatalf("проект %d удалён, а заявки на очистку телеметрии нет", proj.ID)
	}
}

func TestWebDeleteOrg(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	fp := &fakePurger{}
	s.h.Purger = fp

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "delorg-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "delorg-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "delorg-co", "DelOrg Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	deletePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/delete"

	resp := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", deletePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil {
		t.Fatalf("org unexpectedly gone after member-denied delete: %v", err)
	}

	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (owner, unconfirmed) status = %d, want 200: %s", deletePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (owner, unconfirmed) missing confirm page hidden field: %s", deletePath, body)
	}
	if !strings.Contains(string(body), "«DelOrg Co»") {
		t.Fatalf("POST %s (owner, unconfirmed) confirm page does not name the org: %s", deletePath, body)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil {
		t.Fatalf("org unexpectedly gone after unconfirmed delete: %v", err)
	}

	// Роута /orgs нет, поэтому редирект на корень, как у leave-org.
	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", deletePath, resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("POST %s (owner) Location = %q, want /", deletePath, loc)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, ownerID); err == nil {
		t.Fatalf("org still present in PG after delete")
	}
}

func TestWebPurgeSubject(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	fp := &fakePurger{}
	s.h.Purger = fp

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "purge-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "purge-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "purge-co", "Purge Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "purge-proj", "Purge Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	purgePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/purge-subject"
	// confirmed=yes всегда: без него отдаётся страница подтверждения, как для прочих деструктивных действий.
	form := func() url.Values {
		return url.Values{
			"confirmed":  {"yes"},
			"project_id": {strconv.FormatInt(proj.ID, 10)},
			"email":      {"subject@example.com"},
		}
	}

	resp := postForm(t, s.srv, purgePath, form(), "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", purgePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, purgePath, form(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", purgePath, resp.StatusCode)
	}
	if len(fp.subjects) != 0 {
		t.Fatalf("PurgeSubject called on member-denied request: %v", fp.subjects)
	}

	resp = postForm(t, s.srv, purgePath, url.Values{"confirmed": {"yes"}, "project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty subject) status = %d, want 422", purgePath, resp.StatusCode)
	}
	if len(fp.subjects) != 0 {
		t.Fatalf("PurgeSubject called on empty subject: %v", fp.subjects)
	}

	unconfirmed := form()
	unconfirmed.Del("confirmed")
	resp = postForm(t, s.srv, purgePath, unconfirmed, s.srv.URL, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (unconfirmed) status = %d, want 200", purgePath, resp.StatusCode)
	}
	if !strings.Contains(string(cbody), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (unconfirmed) не отдал страницу подтверждения", purgePath)
	}
	if len(fp.subjects) != 0 {
		t.Fatalf("PurgeSubject вызван без подтверждения: %v", fp.subjects)
	}

	resp = postForm(t, s.srv, purgePath, form(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", purgePath, resp.StatusCode)
	}
	if len(fp.subjects) != 1 {
		t.Fatalf("PurgeSubject calls = %d, want 1", len(fp.subjects))
	}
	call := fp.subjects[0]
	if call.projectID != proj.ID || call.sub.Email != "subject@example.com" {
		t.Fatalf("PurgeSubject call = %+v, want projectID=%d email=subject@example.com", call, proj.ID)
	}

	fp.subjectErr = errors.New("clickhouse down")
	resp = postForm(t, s.srv, purgePath, form(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST %s (purge error) status = %d, want 500", purgePath, resp.StatusCode)
	}
}

// total в аудит-логе включает и спаны, которых нет в перечислении по видам.
func TestWebPurgeSubjectAuditLogListsSpans(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	fp := &fakePurger{subjectResult: telemetry.PurgeResult{
		Events: 1, Transactions: 2, Spans: 3, MetricPoints: 4, Logs: 5,
	}}
	s.h.Purger = fp

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "purge-spans-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "purge-spans-co", "Purge Spans Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "purge-spans-proj", "Purge Spans Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	purgePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/purge-subject"
	resp := postForm(t, s.srv, purgePath, url.Values{
		"confirmed":  {"yes"},
		"project_id": {strconv.FormatInt(proj.ID, 10)},
		"email":      {"subject@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", purgePath, resp.StatusCode)
	}

	logLine := buf.String()
	if !strings.Contains(logLine, "subject data purged") {
		t.Fatalf("аудит-лог очистки ПДн не написан: %s", logLine)
	}
	if !strings.Contains(logLine, "spans=3") {
		t.Fatalf("аудит-лог не перечисляет спаны (spans=3): %s", logLine)
	}
	if !strings.Contains(logLine, "total=15") {
		t.Fatalf("аудит-лог: total не совпадает с суммой видов (1+2+3+4+5=15): %s", logLine)
	}
}

func TestWebExportSubject(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	fp := &fakePurger{}
	s.h.Purger = fp

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "export-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "export-member@example.com")
	otherID, otherCookie := orgSettingsRegister(t, authSvc, "export-other@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "export-co", "Export Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "export-proj", "Export Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	other, err := orgSvc.CreateOrg(context.Background(), "export-other-co", "Export Other Co", otherID)
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	otherProj, err := orgSvc.CreateProject(context.Background(), other.ID, "export-other-proj", "Export Other Proj", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	_ = otherCookie

	exportPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/export-subject"
	form := func() url.Values {
		return url.Values{
			"project_id": {strconv.FormatInt(proj.ID, 10)},
			"email":      {"subject@example.com"},
		}
	}

	resp := postForm(t, s.srv, exportPath, form(), "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", exportPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, exportPath, form(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", exportPath, resp.StatusCode)
	}
	if len(fp.exports) != 0 {
		t.Fatalf("ExportSubject called on member-denied request: %v", fp.exports)
	}

	resp = postForm(t, s.srv, exportPath, url.Values{
		"project_id": {strconv.FormatInt(otherProj.ID, 10)},
		"email":      {"subject@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (cross-org project) status = %d, want 404", exportPath, resp.StatusCode)
	}
	if len(fp.exports) != 0 {
		t.Fatalf("ExportSubject called on cross-org project: %v", fp.exports)
	}

	resp = postForm(t, s.srv, exportPath, url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty subject) status = %d, want 422", exportPath, resp.StatusCode)
	}
	if len(fp.exports) != 0 {
		t.Fatalf("ExportSubject called on empty subject: %v", fp.exports)
	}

	resp = postForm(t, s.srv, exportPath, form(), s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (owner) status = %d, want 200", exportPath, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("POST %s (owner) Content-Type = %q, want application/json", exportPath, ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="subject-export.json"` {
		t.Fatalf("POST %s (owner) Content-Disposition = %q", exportPath, cd)
	}
	if len(body) == 0 {
		t.Fatalf("POST %s (owner) empty body", exportPath)
	}
	if len(fp.exports) != 1 {
		t.Fatalf("ExportSubject calls = %d, want 1", len(fp.exports))
	}
	if c := fp.exports[0]; c.projectID != proj.ID || c.sub.Email != "subject@example.com" {
		t.Fatalf("ExportSubject call = %+v, want projectID=%d email=subject@example.com", c, proj.ID)
	}
}
