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
	flow.Provider = "oidc"
	flow.State = "STATE"
	flow.IssuedAt = time.Now().Unix()
	raw, err := signFlow([]byte("test-secret"), flow)
	if err != nil {
		t.Fatalf("signFlow: %v", err)
	}
	req, _ := http.NewRequest("GET", s.srv.URL+"/auth/oauth/oidc/callback?state=STATE&code=CODE", nil)
	req.AddCookie(&http.Cookie{Name: oauthCookieName, Value: raw})
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
	s.mp.id = oauth.Identity{Subject: "sub-6", Email: "different@provider.com", EmailVerified: true}
	resp := s.doCallback(t, oauthFlow{Link: true, UID: uid})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profile" {
		t.Fatalf("link flow status/loc = %d/%s, want 303 /profile", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got, _ := s.auth.IdentityUser(ctx, "oidc", "sub-6"); got != uid {
		t.Fatal("link flow did not link identity")
	}
}
