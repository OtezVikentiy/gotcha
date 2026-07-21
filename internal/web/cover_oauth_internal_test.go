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
)

// emptyAuthURLProvider — провайдер, чей AuthURL пуст: oauthStart должен ответить
// 502 (провайдер недоступен), а не редиректить в никуда.
type emptyAuthURLProvider struct{}

func (emptyAuthURLProvider) Name() string                     { return "empty" }
func (emptyAuthURLProvider) DisplayName() string              { return "Empty" }
func (emptyAuthURLProvider) AuthURL(_, _, _, _ string) string { return "" }
func (emptyAuthURLProvider) Exchange(_ context.Context, _, _, _, _ string) (oauth.Identity, error) {
	return oauth.Identity{}, nil
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// TestCoverOAuthStartHappyPath — GET /auth/oauth/oidc/start выставляет
// flow-cookie и редиректит на страницу согласия провайдера (весь happy-path
// oauthStart: state/nonce/PKCE/signFlow/SetCookie/redirect).
func TestCoverOAuthStartHappyPath(t *testing.T) {
	s := newCallbackStack(t)
	resp, err := noRedirect().Get(s.srv.URL + "/auth/oauth/oidc/start")
	if err != nil {
		t.Fatalf("GET start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start status = %d, want 303", resp.StatusCode)
	}
	var hasCookie bool
	for _, ck := range resp.Cookies() {
		if ck.Name == oauthCookieName && ck.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("start did not set flow cookie")
	}
}

// TestCoverOAuthStartLinkNoSession — GET /auth/oauth/oidc/start?link=1 без
// активной сессии → 303 /login (поток привязки требует залогиненного юзера).
func TestCoverOAuthStartLinkNoSession(t *testing.T) {
	s := newCallbackStack(t)
	resp, err := noRedirect().Get(s.srv.URL + "/auth/oauth/oidc/start?link=1")
	if err != nil {
		t.Fatalf("GET start link: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("start?link=1 no session status/loc = %d/%s, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestCoverOAuthStartEmptyAuthURL — провайдер с пустым AuthURL → 502.
func TestCoverOAuthStartEmptyAuthURL(t *testing.T) {
	h := New(nil, nil, nil, nil, "http://localhost:8080")
	h.SecretKey = "test-secret"
	h.OAuth = oauth.NewRegistry(emptyAuthURLProvider{})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := noRedirect().Get(srv.URL + "/auth/oauth/empty/start")
	if err != nil {
		t.Fatalf("GET start empty: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("start (empty authURL) status = %d, want 502", resp.StatusCode)
	}
}

// TestCoverOAuthCallbackNoCode — callback без параметра code → oauthFail (502).
func TestCoverOAuthCallbackNoCode(t *testing.T) {
	s := newCallbackStack(t)
	flow := oauthFlow{Provider: "oidc", State: "STATE", IssuedAt: time.Now().Unix()}
	raw, err := signFlow([]byte("test-secret"), flow)
	if err != nil {
		t.Fatalf("signFlow: %v", err)
	}
	req, _ := http.NewRequest("GET", s.srv.URL+"/auth/oauth/oidc/callback?state=STATE", nil)
	req.AddCookie(&http.Cookie{Name: oauthCookieName, Value: raw})
	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("callback (no code) status = %d, want 502", resp.StatusCode)
	}
}

// TestCoverOAuthCallbackExchangeError — обмен кода провалился → oauthFail (502).
func TestCoverOAuthCallbackExchangeError(t *testing.T) {
	s := newCallbackStack(t)
	s.mp.err = errors.New("exchange boom")
	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("callback (exchange error) status = %d, want 502", resp.StatusCode)
	}
}

// TestCoverOAuthCallbackLinkAlreadyLinked — юзер уже имеет провайдера oidc
// (другой субъект); поток привязки нового субъекта → UNIQUE (user_id,provider)
// → ErrAlreadyLinked → редирект /profile (без ошибки).
func TestCoverOAuthCallbackLinkAlreadyLinked(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	uid, err := s.auth.Register(ctx, "already@corp.com", "password12")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Уже привязанный oidc с субъектом sub-existing.
	if err := s.auth.LinkIdentity(ctx, uid, "oidc", "sub-existing", "already@corp.com"); err != nil {
		t.Fatalf("link existing: %v", err)
	}
	token, err := s.auth.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	// Новый, ещё не привязанный субъект (IdentityUser → ErrNoIdentity, дальше link).
	s.mp.id = oauth.Identity{Subject: "sub-new", Email: "already@corp.com", EmailVerified: true}
	resp := s.doCallbackSession(t, oauthFlow{Link: true, UID: uid}, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profile" {
		t.Fatalf("callback (already linked) status/loc = %d/%s, want 303 /profile",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestCoverOAuthProvisionUnverifiedEmail — неизвестный email c EmailVerified=false
// уходит в oauthProvisionByInvite, где отвергается с 403 (provider_no_email).
func TestCoverOAuthProvisionUnverifiedEmail(t *testing.T) {
	s := newCallbackStack(t)
	s.mp.id = oauth.Identity{Subject: "sub-unverified-new", Email: "unknown@corp.com", EmailVerified: false}
	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("callback (unverified unknown email) status = %d, want 403", resp.StatusCode)
	}
	if _, err := s.auth.UserByEmail(context.Background(), "unknown@corp.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatal("no user must be created for unverified unknown email")
	}
}
