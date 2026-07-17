package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// doCallback выставляет валидную flow-cookie и дёргает callback без follow-redirect.
func (s *callbackStack) doCallback(t *testing.T, flow oauthFlow) *http.Response {
	t.Helper()
	return s.doCallbackSession(t, flow, "")
}

// doCallbackSession — как doCallback, но дополнительно кладёт сессионную cookie
// (для потока привязки из профиля, где линковка идёт к uid из сессии, а не из flow).
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
	s.mp.id = oauth.Identity{Subject: "sub-1", Email: "u@corp.com", EmailVerified: true}

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

func TestCallbackLinkFlow(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	uid, _ := s.auth.Register(ctx, "linker@corp.com", "password12")
	// SEC-C1b: линковка идёт к uid из сессии, поэтому нужна валидная сессионная cookie.
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

// TestOAuthCallback_EnforcedSSOBlocksEnvProvider — SEC-H2: env-провайдер (личный/
// инстансовый Яндекс/VK/OIDC) не должен выдавать сессию для домена с enforced-SSO.
// Раньше существующий юзер такого домена логинился по ветке 3; теперь — редирект на
// /sso, сессия НЕ выдаётся, identity не привязывается.
func TestOAuthCallback_EnforcedSSOBlocksEnvProvider(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	// enforced-SSO орг для corp.com.
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
	// Существующий юзер этого домена (branch 3 раньше линковал и логинил).
	uid, _ := s.auth.Register(ctx, "worker@corp.com", "password12")
	// env-провайдер "oidc" возвращает verified email того же домена, субъект неизвестен.
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
	// Identity не должна быть привязана к юзеру.
	if got, err := s.auth.IdentityUser(ctx, "oidc", "env-sub"); err == nil {
		t.Fatalf("identity linked (uid %d) despite enforced sso; must not link (user=%d)", got, uid)
	}
}

// TestOAuthCallback_EnforcedSSOUnverifiedEmail — RA-L2: guard enforced-SSO не
// должен зависеть от id.EmailVerified. Generic-OIDC без email_verified
// (EmailVerified=false) на enforced-домене раньше проскакивал мимо гейта и мог
// войти по стабильному субъекту; теперь — редирект на /sso, сессия НЕ выдаётся,
// вход по субъекту не происходит.
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
	// Существующий юзер enforced-домена с УЖЕ привязанной identity: без гейта
	// ветка «login by subject» выдала бы сессию, несмотря на unverified email.
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

// TestOAuthCallback_LinkIgnoresForgedFlowUID — SEC-C1b: подделанный flow.UID (жертвы)
// без активной сессии атакующего НЕ должен приводить к линковке identity к жертве.
// При утёкшем/дефолтном ключе подписи UID в cookie подделывается; доверяем только сессии.
func TestOAuthCallback_LinkIgnoresForgedFlowUID(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	// Жертва с реальным uid, который атакующий подставляет в flow.UID.
	victimUID, _ := s.auth.Register(ctx, "victim@corp.com", "password12")
	// Identity атакующего: субъект неизвестен (IdentityUser → ErrNoIdentity).
	s.mp.id = oauth.Identity{Subject: "attacker-sub", Email: "attacker@evil.com", EmailVerified: true}

	// Сессии нет (doCallback не кладёт сессионную cookie) — линковки быть не должно.
	resp := s.doCallback(t, oauthFlow{Link: true, UID: victimUID})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("forged link without session: status/loc = %d/%s, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	// Identity атакующего не должна быть привязана вообще (тем более к жертве).
	if got, err := s.auth.IdentityUser(ctx, "oidc", "attacker-sub"); err == nil {
		t.Fatalf("forged flow.UID linked identity to uid %d (victim=%d); must not link", got, victimUID)
	}
}
