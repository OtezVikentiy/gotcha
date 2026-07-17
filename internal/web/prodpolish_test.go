package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebIssuesEmptyStateCTA — PROD-P4: пустой список issues показывает не
// сухое «не найдено», а призыв подключить DSN проекта со ссылкой на страницу
// setup конкретного проекта.
func TestWebIssuesEmptyStateCTA(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "empty-cta-owner@example.com")
	project := createProject(t, s, ownerID, "empty-cta-org", "empty-cta-proj")

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	setupPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/setup"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), setupPath) {
		t.Fatalf("GET %s (empty) missing setup CTA link %q: %s", issuesPath, setupPath, body)
	}
	if !strings.Contains(string(body), "Подключите DSN") {
		t.Fatalf("GET %s (empty) missing CTA text: %s", issuesPath, body)
	}
}

// TestWebProjectSettingsRevokeConfirm — под CSP default-src 'self' без
// unsafe-inline инлайновый onclick="confirm()" не исполняется (см. коммит
// «server-side confirm for destructive actions»), поэтому подтверждение
// отзыва ключа — server-side двухшаговый POST: без confirmed=yes revoke
// рендерит страницу подтверждения (200, с сообщением «получат 403» и
// hidden-полем confirmed=yes) и НЕ отзывает ключ; с confirmed=yes — отзывает
// (303). Разметка кнопки Revoke больше не содержит confirm(...).
func TestWebProjectSettingsRevokeConfirm(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "revoke-confirm-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "revoke-confirm-co", "Revoke Confirm Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "revoke-confirm-proj", "Revoke Confirm Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	keysPath := settingsPath + "/keys"
	revokePath := keysPath + "/revoke"

	// Создаём живой ключ, чтобы в таблице появилась кнопка Revoke.
	resp := postForm(t, s.srv, keysPath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (create key) status = %d, want 303", keysPath, resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "confirm(") {
		t.Fatalf("GET %s revoke button still carries dead inline confirm(): %s", settingsPath, body)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("KeysForProject = %+v, err=%v, want 1 key", keys, err)
	}
	keyID := keys[0].ID

	// POST revoke БЕЗ confirmed=yes → 200, страница подтверждения (сообщение
	// «получат 403» и hidden confirmed=yes), ключ НЕ отозван.
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(keyID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (unconfirmed) status = %d, want 200: %s", revokePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "получат 403") {
		t.Fatalf("POST %s (unconfirmed) missing confirm message: %s", revokePath, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (unconfirmed) missing confirmed hidden field: %s", revokePath, body)
	}
	if keys, err := orgSvc.KeysForProject(context.Background(), proj.ID); err != nil || keys[0].Revoked {
		t.Fatalf("key revoked by unconfirmed POST: %+v err=%v", keys, err)
	}

	// POST revoke с confirmed=yes → 303, ключ отозван.
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(keyID, 10)}, "confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (confirmed) status = %d, want 303", revokePath, resp.StatusCode)
	}
	if keys, err := orgSvc.KeysForProject(context.Background(), proj.ID); err != nil || !keys[0].Revoked {
		t.Fatalf("key not revoked after confirmed POST: %+v err=%v", keys, err)
	}
}

// TestWebProjectSettingsRetentionNotice — PROD-P6: при заданном RetentionDays
// страница настроек проекта показывает подпись «События хранятся N дней»; при
// 0 подпись не рендерится.
func TestWebProjectSettingsRetentionNotice(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "retention-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "retention-co", "Retention Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "retention-proj", "Retention Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"

	// RetentionDays не задан → подписи нет.
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "хранятся") {
		t.Fatalf("GET %s shows retention notice with RetentionDays=0: %s", settingsPath, body)
	}

	// RetentionDays=30 → подпись с числом.
	s.h.RetentionDays = 30
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "хранятся") || !strings.Contains(string(body), "30") {
		t.Fatalf("GET %s missing retention notice: %s", settingsPath, body)
	}
}

// TestWebOrgSettingsLeave — PROD-P7: обычный участник покидает организацию сам
// (303, членства больше нет); единственный owner получает 422 (ErrLastOwner).
func TestWebOrgSettingsLeave(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "leave-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "leave-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "leave-co", "Leave Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	leavePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/leave"

	// POST leave без Origin → 403.
	resp := postForm(t, s.srv, leavePath, url.Values{}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", leavePath, resp.StatusCode)
	}

	// POST leave БЕЗ confirmed=yes → 200, страница подтверждения (двухшаговый
	// POST — CSP default-src 'self' без unsafe-inline не исполняет inline
	// onsubmit="confirm()"), членство member НЕ тронуто.
	resp = postForm(t, s.srv, leavePath, url.Values{}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (member, unconfirmed) status = %d, want 200: %s", leavePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (member, unconfirmed) missing confirm page hidden field: %s", leavePath, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleMember {
		t.Fatalf("member role after unconfirmed leave = %v, %v, want member, nil", role, err)
	}

	// Обычный member выходит сам (confirmed=yes) → 303, членства больше нет.
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (member) status = %d, want 303", leavePath, resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, memberID); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("member role after leave: got %v, want ErrNotMember", err)
	}

	// Единственный owner пытается уйти (confirmed=yes) → 422 (ErrLastOwner), членство сохранено.
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (last owner) status = %d, want 422: %s", leavePath, resp.StatusCode, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil || role != org.RoleOwner {
		t.Fatalf("owner role after blocked leave = %v, %v, want owner, nil", role, err)
	}

	// Не участник (посторонний), confirmed=yes → 404.
	_, strangerCookie := orgSettingsRegister(t, authSvc, "leave-stranger@example.com")
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (stranger) status = %d, want 404", leavePath, resp.StatusCode)
	}
}
