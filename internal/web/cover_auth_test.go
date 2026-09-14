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

func TestCoverSSOPageAndSubmit(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

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

	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"a@corp.com"}}, "", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /sso (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"nobody@unknown-domain.example"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /sso (unknown domain) status = %d, want 422", resp.StatusCode)
	}

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

func TestCoverRegistrationClosed(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)

	if _, err := authSvc.Register(context.Background(), "bootstrap@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("register bootstrap: %v", err)
	}
	s.h.RegistrationMode = "invite"

	resp, err := http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /register (closed) status = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/register", url.Values{
		"email": {"second@example.com"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /register (closed, second user) status = %d, want 403", resp.StatusCode)
	}
}

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

func TestCoverEnforcedSSOBlocksPasswordAuth(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, err := authSvc.Register(context.Background(), "enforced-owner@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	enforcedSSOOrg(t, orgSvc, ownerID, "enforced-co", "enforced.example")

	resp := postForm(t, s.srv, "/login", url.Values{
		"email": {"worker@enforced.example"}, "password": {"whatever-password"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /login (enforced sso) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/register", url.Values{
		"email": {"newbie@enforced.example"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /register (enforced sso) status = %d, want 422", resp.StatusCode)
	}
}

func TestCoverProfileIdentityUnlink(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	s.h.OAuth = oauth.NewRegistry(staticProvider{name: "oidc", display: "OIDC", authBase: "https://idp/authorize"})
	ctx := context.Background()

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

	resp := getWithCookie(t, s.srv, "/profile", oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /profile (oauth-only) status = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, "", oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST unlink (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, oauthCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST unlink (last method) status = %d, want 409", resp.StatusCode)
	}

	pwUID, pwCookie := orgSettingsRegister(t, authSvc, "profile-unlink@example.com")
	if err := authSvc.LinkIdentity(ctx, pwUID, "oidc", "sub-pw", "profile-unlink@example.com"); err != nil {
		t.Fatalf("link identity pw user: %v", err)
	}
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"nonexistent"}, "confirmed": {"yes"}}, s.srv.URL, pwCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST unlink (not linked) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}, "confirmed": {"yes"}}, s.srv.URL, pwCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST unlink (success) status = %d, want 200", resp.StatusCode)
	}
	if _, err := authSvc.IdentityUser(ctx, "oidc", "sub-pw"); err != auth.ErrNoIdentity {
		t.Fatalf("identity must be gone after confirmed unlink: %v", err)
	}
}

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

	resp := postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST pwset (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"different-strong-1"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (mismatch) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"short"}, "new2": {"short"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (weak) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST pwset (success) status = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"a-strong-password"}, "new2": {"a-strong-password"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST pwset (already set) status = %d, want 422", resp.StatusCode)
	}
}

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

func TestCoverOnboarding(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "onboard@example.com")

	resp := getWithCookie(t, s.srv, "/onboarding", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /onboarding (no org) status = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co"}, "org_name": {"OB"}, "project_slug": {"ob-proj"}, "project_name": {"P"}, "platform": {"go"},
	}, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /onboarding (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"Bad Slug!"}, "org_name": {"OB"}, "project_slug": {"ob-proj"}, "project_name": {"P"}, "platform": {"weird-platform"},
	}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (bad slug) status = %d, want 422", resp.StatusCode)
	}

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

	resp = getWithCookie(t, s.srv, "/onboarding", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /onboarding (has project) status = %d, want 303", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/register", url.Values{
		"email": {"ob-second@example.com"}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	secondCookie := sessionCookie(resp)
	if secondCookie == nil {
		t.Fatalf("register (second user) did not set session cookie")
	}
	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co"}, "org_name": {"OB2"}, "project_slug": {"ob-proj2"}, "project_name": {"P2"}, "platform": {"php"},
	}, s.srv.URL, secondCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (dup slug) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/onboarding", url.Values{
		"org_slug": {"ob-co-3"}, "org_name": {"OB3"}, "project_slug": {"ob-proj3"}, "project_name": {"P3"}, "platform": {"php"},
	}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /onboarding (has project) status = %d, want 303", resp.StatusCode)
	}

	setupPath := setupLoc

	resp = getWithCookie(t, s.srv, setupPath, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "sentry.Init") {
		t.Fatalf("GET %s missing go snippet: %s", setupPath, body)
	}
	if !strings.Contains(string(body), "go get github.com/getsentry/sentry-go") {
		t.Fatalf("GET %s missing install command: %s", setupPath, body)
	}

	resp = getWithCookie(t, s.srv, "/projects/not-a-number/setup", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /projects/not-a-number/setup status = %d, want 404", resp.StatusCode)
	}

	_, otherCookie := orgSettingsRegister(t, authSvc, "onboard-other@example.com")
	resp = getWithCookie(t, s.srv, setupPath, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", setupPath, resp.StatusCode)
	}
}
