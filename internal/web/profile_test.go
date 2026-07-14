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

// TestWebProfilePassword — задача 4: GET /profile отдаёт форму, POST
// /profile/password проверяет старый пароль и совпадение нового,
// auth.ChangePassword гасит все сессии, но хендлер тут же выпускает новую
// и переустанавливает cookie — юзер остаётся залогинен, а старая cookie
// (и старый пароль) мертвы.
func TestWebProfilePassword(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	_, cookie := orgSettingsRegister(t, authSvc, "profile-pw@example.com")

	// GET /profile -> 200, email и форма.
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

	// POST /profile/password без Origin -> 403.
	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "new-correct-horse", "new-correct-horse"), "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/password (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Неверный старый пароль -> 422.
	resp = postForm(t, s.srv, "/profile/password", pwForm("wrong-password", "new-correct-horse", "new-correct-horse"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (wrong old) status = %d, want 422: %s", resp.StatusCode, body)
	}

	// new != new2 -> 422.
	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "new-correct-horse", "different-value"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (mismatch) status = %d, want 422: %s", resp.StatusCode, body)
	}

	// Слабый новый пароль -> 422.
	resp = postForm(t, s.srv, "/profile/password", pwForm("correct-horse-battery", "short", "short"), s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /profile/password (weak) status = %d, want 422: %s", resp.StatusCode, body)
	}

	// Ни одна из неудачных попыток не должна была менять пароль или cookie.
	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("old password should still work after failed attempts: %v", err)
	}

	// Успешная смена пароля -> 200, сообщение, новая cookie.
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

	// Старый пароль больше не работает, новый — работает.
	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "correct-horse-battery"); err == nil {
		t.Fatalf("old password still works after change")
	}
	if _, err := authSvc.Authenticate(context.Background(), "profile-pw@example.com", "new-correct-horse"); err != nil {
		t.Fatalf("new password does not work: %v", err)
	}

	// Старая cookie мертва (ChangePassword гасит все сессии).
	resp = getWithCookie(t, s.srv, "/profile", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /profile (old cookie) status = %d, want 303", resp.StatusCode)
	}

	// Новая cookie жива.
	resp = getWithCookie(t, s.srv, "/profile", newCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (new cookie) status = %d, want 200: %s", resp.StatusCode, body)
	}
}

// TestWebProfilePasswordRateLimit — security fix (задача 5/3): без лимита
// украденная cookie позволяет перебирать текущий пароль неограниченно. Шесть
// POST /profile/password с неверным старым паролем подряд — шестой должен
// получить 429 (тот же лимит 5/минуту, что и у /login, но отдельное
// ключевое пространство "pw|"+uid).
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

	// Правильный пароль по-прежнему работает (лимит не оставил пользователя
	// без возможности сменить пароль навсегда, только на текущее окно).
	if _, err := authSvc.Authenticate(context.Background(), "profile-pw-ratelimit@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("original password should still work: %v", err)
	}
}

// TestWebProfileSessionsRevoke — задача 4: залогинившись дважды (два
// токена/устройства), POST /profile/sessions/revoke с первого токена гасит
// второй, но не первый, и показывает счётчик удалённых сессий.
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

	// POST /profile/sessions/revoke без Origin -> 403.
	resp := postForm(t, s.srv, "/profile/sessions/revoke", url.Values{}, "", cookieA)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /profile/sessions/revoke (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Оба токена живы до revoke.
	resp = getWithCookie(t, s.srv, "/profile", cookieB)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (cookieB before revoke) status = %d, want 200", resp.StatusCode)
	}

	// Revoke с cookieA (текущий токен сохраняется).
	resp = postForm(t, s.srv, "/profile/sessions/revoke", url.Values{}, s.srv.URL, cookieA)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /profile/sessions/revoke status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "1") {
		t.Fatalf("POST /profile/sessions/revoke body missing revoked count: %s", body)
	}

	// cookieB теперь мертва, cookieA жива.
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

// TestWebIndexNoAccessibleProjects — задача 4: юзер-member организации, у
// которой есть проекты, но сам юзер не привязан ни к одной команде, видит
// стилизованную страницу «нет доступных проектов» (не редирект на
// /onboarding — своей организации у него уже достаточно). Юзер вовсе без
// организаций по-прежнему уходит на /onboarding.
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

	// Юзер без организаций по-прежнему уходит на /onboarding.
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
