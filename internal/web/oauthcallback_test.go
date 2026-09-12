package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type mockProvider struct {
	name string
	id   oauth.Identity
	err  error
}

func (m *mockProvider) Name() string                     { return m.name }
func (m *mockProvider) DisplayName() string              { return m.name }
func (m *mockProvider) AuthURL(_, _, _, _ string) string { return "https://idp/authorize" }
func (m *mockProvider) Exchange(_ context.Context, _, _, _, _ string) (oauth.Identity, error) {
	return m.id, m.err
}

type callbackStack struct {
	h    *Handler
	srv  *httptest.Server
	auth *auth.Service
	org  *org.Service
	mp   *mockProvider
}

func newCallbackStack(t *testing.T) *callbackStack {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := New(authSvc, orgSvc, nil, nil, "http://localhost:8080")
	h.SecretKey = "test-secret"
	mp := &mockProvider{name: "oidc"}
	h.OAuth = oauth.NewRegistry(mp)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &callbackStack{h: h, srv: srv, auth: authSvc, org: orgSvc, mp: mp}
}

func (s *callbackStack) doCallback(t *testing.T, flow oauthFlow) *http.Response {
	t.Helper()
	return s.doCallbackSession(t, flow, "")
}

func (s *callbackStack) doCallbackSession(t *testing.T, flow oauthFlow, sessionToken string) *http.Response {
	t.Helper()
	flow.Provider = "oidc"
	flow.State = "STATE"
	flow.IssuedAt = time.Now().Unix()
	raw, err := signFlow([]byte("test-secret"), flow)
	if err != nil {
		t.Fatalf("signFlow: %v", err)
	}
	req, _ := http.NewRequest("GET", s.srv.URL+"/auth/oauth/oidc/callback?state=STATE&code=CODE", nil)
	req.AddCookie(&http.Cookie{Name: oauthCookieName, Value: raw})
	if sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionToken})
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	return resp
}

func TestCallbackExistingUserVerifiedEmailLinksAndLogsIn(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	uid, _ := s.auth.Register(ctx, "u@corp.com", "password12")
	s.mp.id = oauth.Identity{Subject: "sub-1", Email: "u@corp.com", EmailVerified: true, TrustedIssuer: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("status/loc = %d/%s, want 303 /", resp.StatusCode, resp.Header.Get("Location"))
	}
	var hasSession bool
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CookieName && ck.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatal("no session cookie")
	}
	if got, err := s.auth.IdentityUser(ctx, "oidc", "sub-1"); err != nil || got != uid {
		t.Fatalf("IdentityUser = (%d,%v), want (%d,nil)", got, err, uid)
	}
}

func TestCallbackExistingUserUntrustedIssuerRefused(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.auth.Register(ctx, "victim@corp.com", "password12")
	s.mp.id = oauth.Identity{Subject: "sub-untrusted", Email: "victim@corp.com", EmailVerified: true, TrustedIssuer: false}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("untrusted-issuer verified email must NOT auto-link/login (got 303)")
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-untrusted"); err == nil {
		t.Fatal("identity must not be linked for untrusted issuer")
	}
}

func TestCallbackUnverifiedEmailRefused(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.auth.Register(ctx, "v@corp.com", "password12")
	s.mp.id = oauth.Identity{Subject: "sub-2", Email: "v@corp.com", EmailVerified: false}
	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("unverified email must NOT auto-link/login (got 303)")
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-2"); err == nil {
		t.Fatal("identity must not be linked for unverified email")
	}
}

func TestCallbackInviteProvisioning(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	ownerID, _ := s.auth.Register(ctx, "owner@corp.com", "password12")
	o, _ := s.org.CreateOrg(ctx, "cb-co", "CB Co", ownerID)
	s.org.Invite(ctx, o.ID, "newbie@corp.com", org.RoleMember)
	s.mp.id = oauth.Identity{Subject: "sub-3", Email: "newbie@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite provisioning status = %d, want 303", resp.StatusCode)
	}
	uid, err := s.auth.UserByEmail(ctx, "newbie@corp.com")
	if err != nil {
		t.Fatalf("provisioned user missing: %v", err)
	}
	if got, _ := s.auth.IdentityUser(ctx, "oidc", "sub-3"); got != uid {
		t.Fatalf("identity not linked to provisioned user")
	}
}

func TestCallbackNoInviteRefused(t *testing.T) {
	s := newCallbackStack(t)
	s.h.RegistrationMode = "invite"
	s.mp.id = oauth.Identity{Subject: "sub-4", Email: "stranger@corp.com", EmailVerified: true}
	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("stranger without invite must be refused")
	}
	if _, err := s.auth.UserByEmail(context.Background(), "stranger@corp.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatal("no user must be created without invite")
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	s := newCallbackStack(t)
	s.mp.id = oauth.Identity{Subject: "sub-5", Email: "x@corp.com", EmailVerified: true}
	flow := oauthFlow{Provider: "oidc", State: "OTHER", IssuedAt: time.Now().Unix()}
	raw, _ := signFlow([]byte("test-secret"), flow)
	req, _ := http.NewRequest("GET", s.srv.URL+"/auth/oauth/oidc/callback?state=STATE&code=C", nil)
	req.AddCookie(&http.Cookie{Name: oauthCookieName, Value: raw})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("state mismatch status = %d, want 400", resp.StatusCode)
	}
}

func TestOAuthCallback_SSOFailureShowsOrgDomainNotInternalName(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	ownerID, _ := s.auth.Register(ctx, "owner3@corp.com", "password12")
	o, err := s.org.CreateOrg(ctx, "sso-co3", "SSO Co3", ownerID)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.org.UpsertSSO(ctx, org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: "s",
		Domain: "corp.com", DefaultRole: "member", Enforced: false,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	providerName := "sso-" + strconv.FormatInt(o.ID, 10)

	flow := oauthFlow{Provider: providerName, State: "STATE", IssuedAt: time.Now().Unix()}
	raw, err := signFlow([]byte("test-secret"), flow)
	if err != nil {
		t.Fatalf("signFlow: %v", err)
	}
	req, _ := http.NewRequest("GET", s.srv.URL+"/auth/oauth/"+providerName+"/callback?state=STATE", nil)
	req.AddCookie(&http.Cookie{Name: oauthCookieName, Value: raw})
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), providerName) {
		t.Fatalf("error page leaks internal provider name %q: %s", providerName, body)
	}
	if !strings.Contains(string(body), "corp.com") {
		t.Fatalf("error page must show org domain corp.com: %s", body)
	}
}

func TestCallbackLinkFlow(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	uid, _ := s.auth.Register(ctx, "linker@corp.com", "password12")
	token, err := s.auth.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "sub-6", Email: "different@provider.com", EmailVerified: true}
	resp := s.doCallbackSession(t, oauthFlow{Link: true, UID: uid}, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profile" {
		t.Fatalf("link flow status/loc = %d/%s, want 303 /profile", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got, _ := s.auth.IdentityUser(ctx, "oidc", "sub-6"); got != uid {
		t.Fatal("link flow did not link identity")
	}
}

func TestOAuthCallback_EnforcedSSOBlocksEnvProvider(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	ownerID, _ := s.auth.Register(ctx, "owner@corp.com", "password12")
	o, err := s.org.CreateOrg(ctx, "sso-co", "SSO Co", ownerID)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.org.UpsertSSO(ctx, org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: "s",
		Domain: "corp.com", DefaultRole: "member", Enforced: true,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	uid, _ := s.auth.Register(ctx, "worker@corp.com", "password12")
	s.mp.id = oauth.Identity{Subject: "env-sub", Email: "worker@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sso" {
		t.Fatalf("enforced sso callback status/loc = %d/%s, want 303 /sso",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CookieName && ck.Value != "" {
			t.Fatal("session cookie must NOT be issued for enforced-sso domain via env provider")
		}
	}
	if got, err := s.auth.IdentityUser(ctx, "oidc", "env-sub"); err == nil {
		t.Fatalf("identity linked (uid %d) despite enforced sso; must not link (user=%d)", got, uid)
	}
}

func TestOAuthCallback_EnforcedSSOUnverifiedEmail(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	ownerID, _ := s.auth.Register(ctx, "owner2@corp.com", "password12")
	o, err := s.org.CreateOrg(ctx, "sso-co2", "SSO Co2", ownerID)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.org.UpsertSSO(ctx, org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: "s",
		Domain: "corp.com", DefaultRole: "member", Enforced: true,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	uid, _ := s.auth.Register(ctx, "worker2@corp.com", "password12")
	if err := s.auth.LinkIdentity(ctx, uid, "oidc", "env-sub-2", "worker2@corp.com"); err != nil {
		t.Fatalf("link identity: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "env-sub-2", Email: "worker2@corp.com", EmailVerified: false}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sso" {
		t.Fatalf("enforced sso (unverified) status/loc = %d/%s, want 303 /sso",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CookieName && ck.Value != "" {
			t.Fatal("session cookie must NOT be issued for enforced-sso domain (unverified email)")
		}
	}
}

func TestOAuthCallback_LinkIgnoresForgedFlowUID(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	victimUID, _ := s.auth.Register(ctx, "victim@corp.com", "password12")
	s.mp.id = oauth.Identity{Subject: "attacker-sub", Email: "attacker@evil.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{Link: true, UID: victimUID})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("forged link without session: status/loc = %d/%s, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	if got, err := s.auth.IdentityUser(ctx, "oidc", "attacker-sub"); err == nil {
		t.Fatalf("forged flow.UID linked identity to uid %d (victim=%d); must not link", got, victimUID)
	}
}

func TestOAuthCallback_LinkRejectsInjectedFlowCookie(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()

	attackerUID, _ := s.auth.Register(ctx, "attacker@evil.com", "password12")
	victimUID, _ := s.auth.Register(ctx, "victim@corp.com", "password12")

	victimToken, err := s.auth.CreateSession(ctx, victimUID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "attacker-sub", Email: "attacker@evil.com", EmailVerified: true}

	resp := s.doCallbackSession(t, oauthFlow{Link: true, UID: attackerUID}, victimToken)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther && resp.Header.Get("Location") == "/profile" {
		t.Fatal("подменённая cookie потока принята: привязка выполнена")
	}
	if got, err := s.auth.IdentityUser(ctx, "oidc", "attacker-sub"); err == nil && got == victimUID {
		t.Fatalf("ЗАХВАТ АККАУНТА: identity атакующего привязана к жертве (uid=%d)", victimUID)
	}
}

func TestOAuthCallback_ClosedModeBlocksInviteProvisioning(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.h.RegistrationMode = "closed"

	ownerID, _ := s.auth.Register(ctx, "closed-owner@corp.com", "password12")
	o, _ := s.org.CreateOrg(ctx, "closed-co", "Closed Co", ownerID)
	if _, err := s.org.Invite(ctx, o.ID, "invited@corp.com", org.RoleMember); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "sub-closed", Email: "invited@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: в closed аккаунт не должен создаваться", resp.StatusCode)
	}
	if _, err := s.auth.UserByEmail(ctx, "invited@corp.com"); err == nil {
		t.Fatal("в режиме closed создан аккаунт по приглашению")
	}
}
