package web_test

import (
	"context"
	"errors"
	"io"
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

// fakePurger реализует web.ProjectPurger без ClickHouse: считает вызовы
// PurgeProject/PurgeSubject, чтобы web-тесты проверяли, что best-effort
// CH-очистка вызвана с нужными аргументами, не поднимая CH-контейнер.
type fakePurger struct {
	mu         sync.Mutex
	projects   []int64
	subjects   []purgeSubjectCall
	exports    []purgeSubjectCall
	subjectErr error // если задан — PurgeSubject возвращает его (тест error-ветки)
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

func (f *fakePurger) PurgeSubject(_ context.Context, projectID int64, sub telemetry.Subject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, purgeSubjectCall{projectID: projectID, sub: sub})
	return f.subjectErr
}

// ExportSubject фиксирует вызов и возвращает заглушку — web-тесту важен только
// факт вызова с нужными projectID/Subject и то, что хендлер отдаёт JSON.
func (f *fakePurger) ExportSubject(_ context.Context, projectID int64, sub telemetry.Subject) (telemetry.SubjectExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exports = append(f.exports, purgeSubjectCall{projectID: projectID, sub: sub})
	return telemetry.SubjectExport{}, nil
}

// TestWebDeleteProject — POST /projects/{id}/settings/delete: owner удаляет
// проект (303, проекта нет в PG, Purger.PurgeProject вызван с projectID);
// member — 404 (owner-only, единый 404 как прочие owner-only действия);
// без Origin — 403.
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

	// POST без Origin → 403.
	resp := postForm(t, s.srv, deletePath, url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", deletePath, resp.StatusCode)
	}

	// POST member (role=member, не owner) → 404, проект жив, Purger не вызван.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}
	if len(fp.projects) != 0 {
		t.Fatalf("PurgeProject called on member-denied request: %v", fp.projects)
	}

	// POST owner БЕЗ confirmed=yes → 200, страница подтверждения (двухшаговый
	// POST — CSP default-src 'self' без unsafe-inline не исполняет inline
	// onsubmit="confirm()"), проект НЕ удалён, Purger не вызван.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (owner, unconfirmed) status = %d, want 200: %s", deletePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (owner, unconfirmed) missing confirm page hidden field: %s", deletePath, body)
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

	// POST owner с confirmed=yes → 303, проект удалён из PG, Purger.PurgeProject вызван с projectID.
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
	if len(fp.projects) != 1 || fp.projects[0] != proj.ID {
		t.Fatalf("PurgeProject calls = %v, want [%d]", fp.projects, proj.ID)
	}
}

// TestWebDeleteOrg — POST /orgs/{id}/settings/delete: owner удаляет орг (303
// на /, орга нет в PG); member — 404; без Origin — 403. CH-очистка орга
// в этой задаче не выполняется (проектов у орга может не быть; телеметрию
// чистит удаление конкретных проектов) — Purger.PurgeProject тут не ждём.
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

	// POST без Origin → 403.
	resp := postForm(t, s.srv, deletePath, url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", deletePath, resp.StatusCode)
	}

	// POST member → 404, орг жив.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil {
		t.Fatalf("org unexpectedly gone after member-denied delete: %v", err)
	}

	// POST owner БЕЗ confirmed=yes → 200, страница подтверждения, орг НЕ удалён.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (owner, unconfirmed) status = %d, want 200: %s", deletePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (owner, unconfirmed) missing confirm page hidden field: %s", deletePath, body)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil {
		t.Fatalf("org unexpectedly gone after unconfirmed delete: %v", err)
	}

	// POST owner с confirmed=yes → 303 на /, орг удалён (Role → ErrNotMember).
	// Роута /orgs нет (RA-7), поэтому редирект на корень, как у leave-org.
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

// TestWebPurgeSubject — POST /orgs/{id}/settings/purge-subject: owner чистит
// ПДн субъекта по проекту (303 обратно на настройки, Purger.PurgeSubject
// вызван с project_id и заполненным Subject); member — 404; пустой субъект →
// 422; без Origin — 403.
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
	form := func() url.Values {
		return url.Values{
			"project_id": {strconv.FormatInt(proj.ID, 10)},
			"email":      {"subject@example.com"},
		}
	}

	// POST без Origin → 403.
	resp := postForm(t, s.srv, purgePath, form(), "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", purgePath, resp.StatusCode)
	}

	// POST member → 404, Purger не вызван.
	resp = postForm(t, s.srv, purgePath, form(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", purgePath, resp.StatusCode)
	}
	if len(fp.subjects) != 0 {
		t.Fatalf("PurgeSubject called on member-denied request: %v", fp.subjects)
	}

	// POST owner с пустым субъектом → 422 (хотя бы одно поле обязательно).
	resp = postForm(t, s.srv, purgePath, url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty subject) status = %d, want 422", purgePath, resp.StatusCode)
	}
	if len(fp.subjects) != 0 {
		t.Fatalf("PurgeSubject called on empty subject: %v", fp.subjects)
	}

	// POST owner с email → 303 обратно на настройки, Purger.PurgeSubject вызван.
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

	// Ошибка удаления НЕ выдаётся за успех (право на удаление ПДн): → 500, а не 303.
	fp.subjectErr = errors.New("clickhouse down")
	resp = postForm(t, s.srv, purgePath, form(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST %s (purge error) status = %d, want 500", purgePath, resp.StatusCode)
	}
}

// TestWebExportSubject — POST /orgs/{id}/settings/export-subject (право субъекта
// на доступ, 152-ФЗ): owner выгружает ПДн субъекта по проекту (200
// application/json + Content-Disposition attachment, Purger.ExportSubject вызван
// с project_id и заполненным Subject); member — 404; project_id чужого орга —
// 404; пустой субъект → 422; без Origin — 403.
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

	// Чужой орг с собственным проектом — для проверки cross-org project_id.
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

	// POST без Origin → 403.
	resp := postForm(t, s.srv, exportPath, form(), "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", exportPath, resp.StatusCode)
	}

	// POST member → 404, Purger не вызван.
	resp = postForm(t, s.srv, exportPath, form(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", exportPath, resp.StatusCode)
	}
	if len(fp.exports) != 0 {
		t.Fatalf("ExportSubject called on member-denied request: %v", fp.exports)
	}

	// POST owner с project_id чужого орга → 404, Purger не вызван.
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

	// POST owner с пустым субъектом → 422.
	resp = postForm(t, s.srv, exportPath, url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty subject) status = %d, want 422", exportPath, resp.StatusCode)
	}
	if len(fp.exports) != 0 {
		t.Fatalf("ExportSubject called on empty subject: %v", fp.exports)
	}

	// POST owner с email → 200 application/json + attachment, Purger.ExportSubject вызван.
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
