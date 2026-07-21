package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// enforcedSSOOrg заводит организацию с enforced-SSO на домене domain — этого
// достаточно, чтобы вход/регистрация паролем по этому домену отвергались.
func enforcedSSOOrg(t *testing.T, orgSvc *org.Service, ownerID int64, slug, domain string) {
	t.Helper()
	o, err := orgSvc.CreateOrg(context.Background(), slug, slug, ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.UpsertSSO(context.Background(), org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp.example/realms/x", ClientID: "cid",
		ClientSecret: "sec", Domain: domain, DefaultRole: "member", Enforced: true,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
}

// TestCoverSSOPageAndSubmit — GET /sso отдаёт форму; POST /sso: без Origin →
// 403, неизвестный домен → 422, настроенный домен → 303 на SSO-start орга,
// исчерпанный лимит → 429.
func TestCoverSSOPageAndSubmit(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	// GET /sso → 200 с формой (identifier-first вход).
	resp, err := http.Get(s.srv.URL + "/sso")
	if err != nil {
		t.Fatalf("GET /sso: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sso status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<form") {
		t.Fatalf("GET /sso missing form: %s", body)
	}

	// POST /sso без Origin → 403.
	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"a@corp.com"}}, "", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /sso (no origin) status = %d, want 403", resp.StatusCode)
	}

	// POST /sso с неизвестным доменом → 422 (нейтральное сообщение).
	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"nobody@unknown-domain.example"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /sso (unknown domain) status = %d, want 422", resp.StatusCode)
	}

	// Настроенный домен → 303 на /auth/oauth/sso-{orgID}/start.
	ownerID, err := authSvc.Register(context.Background(), "sso-page-owner@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	o, err := orgSvc.CreateOrg(context.Background(), "sso-page-co", "SSO Page Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.UpsertSSO(context.Background(), org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp.example/realms/x", ClientID: "cid",
		ClientSecret: "sec", Domain: "corp.com", DefaultRole: "member",
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"worker@corp.com"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sso (configured domain) status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/auth/oauth/sso-"+strconv.FormatInt(o.ID, 10)+"/start" {
		t.Fatalf("POST /sso Location = %q", loc)
	}

	// Исчерпание лимита (5/мин на sso|ip|email): 6-я попытка того же email → 429.
	var last *http.Response
	for i := 0; i < 6; i++ {
		last = postForm(t, s.srv, "/sso", url.Values{"email": {"ratelimit@corp.com"}}, s.srv.URL, nil)
		io.Copy(io.Discard, last.Body)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th POST /sso status = %d, want 429", last.StatusCode)
	}
}

// TestCoverRegistrationClosed — режим invite + уже существующий пользователь:
// GET /register показывает экран «по приглашению» (registrationClosed), а POST
// /register вторым пользователем → 403.
func TestCoverRegistrationClosed(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	// Первый пользователь инстанса уже есть — bootstrap пройден.
	if _, err := authSvc.Register(context.Background(), "bootstrap@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("register bootstrap: %v", err)
	}
	s.h.RegistrationMode = "invite"

	// GET /register → 200, экран закрытой регистрации.
	resp, err := http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /register (closed) status = %d, want 200", resp.StatusCode)
	}

	// POST /register вторым пользователем → 403.
	resp = postForm(t, s.srv, "/register", url.Values{
		"email": {"second@example.com"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /register (closed, second user) status = %d, want 403", resp.StatusCode)
	}
}

// TestCoverRegisterValidationBranches — POST /register: пароли не совпадают,
// слабый пароль, невалидный email → 422 каждый (открытый режим).
func TestCoverRegisterValidationBranches(t *testing.T) {
	s := newStack(t)

	cases := []struct {
		name string
		form url.Values
	}{
		{"mismatch", url.Values{"email": {"m1@example.com"}, "password": {"correct-horse-battery"}, "password2": {"different-value-here"}}},
		{"weak", url.Values{"email": {"m2@example.com"}, "password": {"short"}, "password2": {"short"}}},
		{"invalid-email", url.Values{"email": {"not-an-email"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"}}},
	}
	for _, tc := range cases {
		resp := postForm(t, s.srv, "/register", tc.form, s.srv.URL, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST /register (%s) status = %d, want 422", tc.name, resp.StatusCode)
		}
	}
}

// TestCoverEnforcedSSOBlocksPasswordAuth — SEC-H2: вход и регистрация паролем по
// домену с enforced-SSO → 422 (только вход через SSO).
func TestCoverEnforcedSSOBlocksPasswordAuth(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, err := authSvc.Register(context.Background(), "enforced-owner@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	enforcedSSOOrg(t, orgSvc, ownerID, "enforced-co", "enforced.example")

	// POST /login по enforced-домену → 422.
	resp := postForm(t, s.srv, "/login", url.Values{
		"email": {"worker@enforced.example"}, "password": {"whatever-password"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /login (enforced sso) status = %d, want 422", resp.StatusCode)
	}

	// POST /register по enforced-домену → 422.
	resp = postForm(t, s.srv, "/register", url.Values{
		"email": {"newbie@enforced.example"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /register (enforced sso) status = %d, want 422", resp.StatusCode)
	}
}

// TestCoverProfileIdentityUnlink — POST /profile/identities/unlink: без Origin →
// 403; OAuth-only юзер с единственной привязкой → 409 (последний способ входа);
// юзер с паролем отвязывает несуществующего провайдера → 422; успешная отвязка →
// 200.
func TestCoverProfileIdentityUnlink(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	s.h.OAuth = oauth.NewRegistry(staticProvider{name: "oidc", display: "OIDC", authBase: "https://idp/authorize"})
	ctx := context.Background()

	// OAuth-only юзер с единственной привязкой oidc.
	oauthUID, err := authSvc.CreateOAuthUser(ctx, "oauth-only@example.com")
	if err != nil {
		t.Fatalf("create oauth user: %v", err)
	}
	if err := authSvc.LinkIdentity(ctx, oauthUID, "oidc", "sub-oauth-only", "oauth-only@example.com"); err != nil {
		t.Fatalf("link identity: %v", err)
	}
	oauthToken, err := authSvc.CreateSession(ctx, oauthUID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	oauthCookie := &http.Cookie{Name: auth.CookieName, Value: oauthToken}

	// GET /profile OAuth-only юзером — рендерит linked/linkable секции.
	resp := getWithCookie(t, s.srv, "/profile", oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (oauth-only) status = %d, want 200", resp.StatusCode)
	}

	// Без Origin → 403.
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, "", oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST unlink (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Единственный способ входа → 409.
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST unlink (last method) status = %d, want 409", resp.StatusCode)
	}

	// Юзер с паролем + привязкой oidc: отвязка несуществующего провайдера → 422.
	pwUID, pwCookie := orgSettingsRegister(t, authSvc, "profile-unlink@example.com")
	if err := authSvc.LinkIdentity(ctx, pwUID, "oidc", "sub-pw", "profile-unlink@example.com"); err != nil {
		t.Fatalf("link identity pw user: %v", err)
	}
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"nonexistent"}}, s.srv.URL, pwCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST unlink (not linked) status = %d, want 422", resp.StatusCode)
	}

	// Успешная отвязка (есть пароль как запасной способ) → 200.
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, pwCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST unlink (success) status = %d, want 200", resp.StatusCode)
	}
}

// TestCoverProfilePasswordSet — POST /profile/password/set: без Origin → 403;
// OAuth-only юзер задаёт пароль → 200; несовпадение → 422; уже заданный пароль
// → 422; слабый → 422.
func TestCoverProfilePasswordSet(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	ctx := context.Background()

	oauthUID, err := authSvc.CreateOAuthUser(ctx, "pwset-oauth@example.com")
	if err != nil {
		t.Fatalf("create oauth user: %v", err)
	}
	token, err := authSvc.CreateSession(ctx, oauthUID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	cookie := &http.Cookie{Name: auth.CookieName, Value: token}

	// Без Origin → 403.
	resp := postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST pwset (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Несовпадение → 422.
	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"different-strong-1"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (mismatch) status = %d, want 422", resp.StatusCode)
	}

	// Слабый → 422.
	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"short"}, "new2": {"short"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (weak) status = %d, want 422", resp.StatusCode)
	}

	// Успех → 200.
	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST pwset (success) status = %d, want 200", resp.StatusCode)
	}

	// Пароль уже задан → 422.
	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (already set) status = %d, want 422", resp.StatusCode)
	}
}

// TestCoverProfileSessionsRevokeNoToken — POST /profile/sessions/revoke без
// сессионного токена в cookie... покрыт auth-middleware, но revoke с валидной
// сессией и нулём других сессий рендерит счётчик 0.
func TestCoverProfileSessionsRevokeZero(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "revoke-zero@example.com")

	resp := postForm(t, s.srv, "/profile/sessions/revoke", url.Values{}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST revoke (zero others) status = %d, want 200", resp.StatusCode)
	}
}

// TestCoverOnboarding — GET /onboarding (нет орга → форма; есть проект → 303 /);
// POST /onboarding: без Origin → 403; невалидный slug → 422; успех → 303 setup;
// дубликат slug → 422.
func TestCoverOnboarding(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "onboard@example.com")

	// GET /onboarding без организаций → 200 с формой.
	resp := getWithCookie(t, s.srv, "/onboarding", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /onboarding (no org) status = %d, want 200", resp.StatusCode)
	}

	// POST без Origin → 403.
	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co"}, "org_name": {"OB"}, "project_slug": {"ob-proj"}, "project_name": {"P"}, "platform": {"go"},
	}, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /onboarding (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Невалидный slug → 422.
	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"Bad Slug!"}, "org_name": {"OB"}, "project_slug": {"ob-proj"}, "project_name": {"P"}, "platform": {"weird-platform"},
	}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (bad slug) status = %d, want 422", resp.StatusCode)
	}

	// Успех → 303 на /projects/{id}/setup.
	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co"}, "org_name": {"OB"}, "project_slug": {"ob-proj"}, "project_name": {"P"}, "platform": {"go"},
	}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /onboarding (valid) status = %d, want 303", resp.StatusCode)
	}
	setupLoc := resp.Header.Get("Location")
	if !strings.HasPrefix(setupLoc, "/projects/") || !strings.HasSuffix(setupLoc, "/setup") {
		t.Fatalf("POST /onboarding Location = %q, want /projects/{id}/setup", setupLoc)
	}

	// GET /onboarding теперь → 303 / (у юзера уже есть проект).
	resp = getWithCookie(t, s.srv, "/onboarding", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /onboarding (has project) status = %d, want 303", resp.StatusCode)
	}

	// Дубликат org_slug → 422 (ErrSlugTaken).
	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co"}, "org_name": {"OB2"}, "project_slug": {"ob-proj2"}, "project_name": {"P2"}, "platform": {"php"},
	}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (dup slug) status = %d, want 422", resp.StatusCode)
	}

	// GET /projects/{id}/setup: успех (DSN + сниппеты) и доступы.
	setupPath := setupLoc

	// Успешный рендер setup-страницы владельцем.
	resp = getWithCookie(t, s.srv, setupPath, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "gotcha.Init") {
		t.Fatalf("GET %s missing go snippet: %s", setupPath, body)
	}

	// Невалидный id → 404.
	resp = getWithCookie(t, s.srv, "/projects/not-a-number/setup", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /projects/not-a-number/setup status = %d, want 404", resp.StatusCode)
	}

	// Чужой проект (нет доступа) → 404.
	_, otherCookie := orgSettingsRegister(t, authSvc, "onboard-other@example.com")
	resp = getWithCookie(t, s.srv, setupPath, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", setupPath, resp.StatusCode)
	}
}
