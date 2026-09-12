package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

func TestWebProfilePassword(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	_, cookie := orgSettingsRegister(t, authSvc, "profile-pw@example.com")

	resp := getWithCookie(t, s.srv, "/profile", cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "profile-pw@example.com") {
		t.Fatalf("GET /profile body missing email: %s", body)
	}
	if !strings.Contains(string(body), "<form") {
		t.Fatalf("GET /profile body has no <form: %s", body)
	}

	pwForm := func(old, new1, new2 string) url.Values {
		return url.Values{"old": {old}, "new": {new1}, "new2": {new2}}
	}

	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "new-correct-horse", "new-correct-horse"), "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/password (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/password", pwForm("wrong-password", "new-correct-horse", "new-correct-horse"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (wrong old) status = %d, want 422: %s", resp.StatusCode, body)
	}

	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "new-correct-horse", "different-value"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (mismatch) status = %d, want 422: %s", resp.StatusCode, body)
	}

	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "short", "short"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (weak) status = %d, want 422: %s", resp.StatusCode, body)
	}

	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("old password should still work after failed attempts: %v", err)
	}

	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "new-correct-horse", "new-correct-horse"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /profile/password (success) status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "пароль изменён") {
		t.Fatalf("POST /profile/password (success) body missing confirmation: %s", body)
	}
	newCookie := sessionCookie(resp)
	if newCookie == nil || newCookie.Value == "" {
		t.Fatalf("POST /profile/password (success) did not set a new session cookie")
	}
	if newCookie.Value == cookie.Value {
		t.Fatalf("POST /profile/password (success) reused the old session token")
	}

	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "correct-horse-battery"); err == nil {
		t.Fatalf("old password still works after change")
	}
	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "new-correct-horse"); err != nil {
		t.Fatalf("new password does not work: %v", err)
	}

	resp = getWithCookie(t, s.srv, "/profile", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /profile (old cookie) status = %d, want 303", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, "/profile", newCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (new cookie) status = %d, want 200: %s", resp.StatusCode, body)
	}
}

func TestWebProfilePasswordRateLimit(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	_, cookie := orgSettingsRegister(t, authSvc, "profile-pw-ratelimit@example.com")

	form := url.Values{"old": {"wrong-password"}, "new": {"new-correct-horse"}, "new2": {"new-correct-horse"}}

	var last *http.Response
	for i := 0; i < 6; i++ {
		last = postForm(t, s.srv, "/profile/password", form, s.srv.URL, cookie)
		io.Copy(io.Discard, last.Body)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th POST /profile/password (wrong old password) status = %d, want 429", last.StatusCode)
	}

	if _, err := authSvc.Authenticate(context.Background(), "profile-pw-ratelimit@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("original password should still work: %v", err)
	}
}

func TestWebProfileSessionsRevoke(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	uid, err := authSvc.Register(context.Background(), "profile-revoke@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tokenA, err := authSvc.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session A: %v", err)
	}
	tokenB, err := authSvc.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session B: %v", err)
	}
	cookieA := &http.Cookie{Name: auth.CookieName, Value: tokenA}
	cookieB := &http.Cookie{Name: auth.CookieName, Value: tokenB}

	resp := postForm(t, s.srv, "/profile/sessions/revoke", url.Values{}, "", cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/sessions/revoke (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, "/profile", cookieB)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (cookieB before revoke) status = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/sessions/revoke", url.Values{}, s.srv.URL, cookieA)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /profile/sessions/revoke status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "1") {
		t.Fatalf("POST /profile/sessions/revoke body missing revoked count: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/profile", cookieB)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /profile (cookieB after revoke) status = %d, want 303", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, "/profile", cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (cookieA after revoke) status = %d, want 200", resp.StatusCode)
	}
}

func TestWebIndexNoAccessibleProjects(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "noproj-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "noproj-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "noproj-org", "No Proj Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := orgSvc.CreateProject(context.Background(), o.ID, "proj", "Proj", "go"); err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/", memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / (member, no accessible projects) status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Нет доступных проектов") {
		t.Fatalf("GET / (member, no accessible projects) body missing message: %s", body)
	}

	_, loneCookie := orgSettingsRegister(t, authSvc, "noproj-lonely@example.com")
	resp = getWithCookie(t, s.srv, "/", loneCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / (no orgs) status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/onboarding" {
		t.Fatalf("GET / (no orgs) Location = %q, want /onboarding", got)
	}
}

func TestWebProfileDeleteBlockedForInstanceAdmin(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	uidA, cookieA := orgSettingsRegister(t, authSvc, "instadmin-delete@example.com")
	uidB, _ := orgSettingsRegister(t, authSvc, "instadmin-delete-b@example.com")

	resp := postForm(t, s.srv, "/profile/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, cookieA)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /profile/delete (instance admin) status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Сначала передайте роль") {
		t.Fatalf("POST /profile/delete (instance admin) body missing explanation: %s", body)
	}

	if _, err := authSvc.UserEmail(context.Background(), uidA); err != nil {
		t.Fatalf("UserEmail(A) after blocked delete: %v", err)
	}
	if _, err := authSvc.UserEmail(context.Background(), uidB); err != nil {
		t.Fatalf("UserEmail(B) after blocked delete: %v", err)
	}

	profResp := getWithCookie(t, s.srv, "/profile", cookieA)
	profBody, _ := io.ReadAll(profResp.Body)
	profResp.Body.Close()
	if profResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile после заблокированного удаления status = %d, want 200 (сессия жива): %s",
			profResp.StatusCode, profBody)
	}
}

func TestWebProfileDeleteSoleInstanceAdminSucceeds(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	uidA, cookieA := orgSettingsRegister(t, authSvc, "sole-admin-delete@example.com")

	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin before delete = (%v,%v), want (true,nil)", admin, err)
	}

	resp := postForm(t, s.srv, "/profile/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /profile/delete (sole admin) status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}
	if _, err := authSvc.UserEmail(context.Background(), uidA); err == nil {
		t.Fatalf("A still exists after self-delete as sole instance admin")
	}

	uidC, err := authSvc.Register(context.Background(), "next-after-sole-admin@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register C: %v", err)
	}
	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidC); err != nil || !admin {
		t.Fatalf("C IsInstanceAdmin after sole admin's self-delete = (%v,%v), want (true,nil)", admin, err)
	}
}

func TestWebProfileInstanceAdminTransfer(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	uidA, cookieA := orgSettingsRegister(t, authSvc, "instadmin-a@example.com")
	uidB, _ := orgSettingsRegister(t, authSvc, "instadmin-b@example.com")

	resp := getWithCookie(t, s.srv, "/profile", cookieA)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/profile/instance-admin/transfer") {
		t.Fatalf("GET /profile (A, instance admin) body missing transfer form: %s", body)
	}

	_, cookieB := orgSettingsRegister(t, authSvc, "instadmin-b2@example.com")
	resp = getWithCookie(t, s.srv, "/profile", cookieB)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "/profile/instance-admin/transfer") {
		t.Fatalf("GET /profile (non-admin) body has transfer form, want none: %s", body)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer", url.Values{"email": {"instadmin-b@example.com"}}, "", cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/instance-admin/transfer (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer", url.Values{"email": {"instadmin-b@example.com"}}, s.srv.URL, cookieA)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /profile/instance-admin/transfer (unconfirmed) status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed"`) {
		t.Fatalf("POST /profile/instance-admin/transfer (unconfirmed) body missing confirmation form: %s", body)
	}
	if !strings.Contains(string(body), "instadmin-b@example.com") {
		t.Fatalf("POST /profile/instance-admin/transfer (unconfirmed) body missing target email: %s", body)
	}

	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin before confirm = (%v,%v), want (true,nil)", admin, err)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer",
		url.Values{"email": {"instadmin-a@example.com"}, "confirmed": {"yes"}}, s.srv.URL, cookieA)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/instance-admin/transfer (self) status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Нельзя передать роль администратора инстанса самому себе") {
		t.Fatalf("POST /profile/instance-admin/transfer (self) body missing err_self text: %s", body)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer",
		url.Values{"email": {""}, "confirmed": {"yes"}}, s.srv.URL, cookieA)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/instance-admin/transfer (empty email) status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Укажите email пользователя, которому передаётся роль") {
		t.Fatalf("POST /profile/instance-admin/transfer (empty email) body missing err_email_required text: %s", body)
	}

	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after rejected self/empty transfers = (%v,%v), want (true,nil)", admin, err)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer",
		url.Values{"email": {"instadmin-b@example.com"}, "confirmed": {"yes"}}, s.srv.URL, cookieA)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /profile/instance-admin/transfer (confirmed) status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "передана") {
		t.Fatalf("POST /profile/instance-admin/transfer (confirmed) body missing success message: %s", body)
	}
	if strings.Contains(string(body), "/profile/instance-admin/transfer") {
		t.Fatalf("POST /profile/instance-admin/transfer (confirmed) body still has transfer form for former admin: %s", body)
	}

	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidB); err != nil || !admin {
		t.Fatalf("B IsInstanceAdmin after transfer = (%v,%v), want (true,nil)", admin, err)
	}
	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidA); err != nil || admin {
		t.Fatalf("A IsInstanceAdmin after transfer = (%v,%v), want (false,nil)", admin, err)
	}

	resp = postForm(t, s.srv, "/profile/instance-admin/transfer",
		url.Values{"email": {"instadmin-b@example.com"}, "confirmed": {"yes"}}, s.srv.URL, cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/instance-admin/transfer (former admin) status = %d, want 403", resp.StatusCode)
	}
}

func TestWebProfileInstanceAdminTransferUnknownEmail(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	uidA, cookieA := orgSettingsRegister(t, authSvc, "instadmin-unknown@example.com")

	resp := postForm(t, s.srv, "/profile/instance-admin/transfer",
		url.Values{"email": {"nobody@example.com"}, "confirmed": {"yes"}}, s.srv.URL, cookieA)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/instance-admin/transfer (unknown email) status = %d, want 422: %s", resp.StatusCode, body)
	}

	if admin, err := authSvc.UserIsInstanceAdmin(context.Background(), uidA); err != nil || !admin {
		t.Fatalf("A IsInstanceAdmin after failed transfer = (%v,%v), want (true,nil)", admin, err)
	}
}
