package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// oauthProfileStack — стенд профиля с включёнными провайдерами (h.OAuth).
type oauthProfileStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	auth *auth.Service
}

func newOAuthProfileStack(t *testing.T, providers ...oauth.Provider) *oauthProfileStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if len(providers) > 0 {
		h.OAuth = oauth.NewRegistry(providers...)
	}
	h.Register(mux)
	return &oauthProfileStack{pool: pool, srv: srv, auth: authSvc}
}

// loginCookie выпускает сессию для uid и возвращает cookie.
func loginCookie(t *testing.T, authSvc *auth.Service, uid int64) *http.Cookie {
	t.Helper()
	token, err := authSvc.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: token}
}

func TestProfileShowsLinkedAndLinkable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s := newOAuthProfileStack(t,
		staticProvider{name: "oidc", display: "OIDC", authBase: "https://idp/authorize"},
		staticProvider{name: "yandex", display: "Яндекс", authBase: "https://ya/authorize"},
	)
	ctx := context.Background()
	uid, err := s.auth.Register(ctx, "prof@example.com", "password12")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.auth.LinkIdentity(ctx, uid, "oidc", "sub-1", "prof@example.com"); err != nil {
		t.Fatalf("link: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	resp := getWithCookie(t, s.srv, "/profile", cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bs := string(body)
	// Привязанный oidc виден, есть кнопка «Отвязать» (пароль есть → CanUnlink).
	if !strings.Contains(bs, "Отвязать") {
		t.Fatalf("profile missing unlink button: %s", bs)
	}
	// yandex ещё не привязан → предлагается «Привязать».
	if !strings.Contains(bs, "/auth/oauth/yandex/start?link=1") {
		t.Fatalf("profile missing linkable yandex: %s", bs)
	}
}

func TestProfileUnlinkLastMethodBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s := newOAuthProfileStack(t, staticProvider{name: "oidc", display: "OIDC", authBase: "https://idp/authorize"})
	ctx := context.Background()
	uid, err := s.auth.CreateOAuthUser(ctx, "oauthonly@example.com")
	if err != nil {
		t.Fatalf("create oauth user: %v", err)
	}
	if err := s.auth.LinkIdentity(ctx, uid, "oidc", "sub-1", "oauthonly@example.com"); err != nil {
		t.Fatalf("link: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	// Единственный способ входа → 409, привязка на месте.
	resp := postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlink last method status = %d, want 409", resp.StatusCode)
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-1"); err != nil {
		t.Fatalf("identity must remain after blocked unlink: %v", err)
	}

	// Задать пароль → теперь отвязка разрешена.
	if err := s.auth.SetPassword(ctx, uid, "newpassword12"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlink after password status = %d, want 200", resp.StatusCode)
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-1"); err != auth.ErrNoIdentity {
		t.Fatalf("identity must be gone after unlink: %v", err)
	}
}

func TestProfilePasswordSet(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s := newOAuthProfileStack(t, staticProvider{name: "oidc", display: "OIDC", authBase: "https://idp/authorize"})
	ctx := context.Background()
	uid, err := s.auth.CreateOAuthUser(ctx, "setpw@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	// Несовпадение → 422, пароль не задан.
	resp := postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"password12"}, "new2": {"different12"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch status = %d, want 422", resp.StatusCode)
	}
	if has, _ := s.auth.HasPassword(ctx, uid); has {
		t.Fatal("password must not be set on mismatch")
	}
	// Совпадение → пароль задан, логин паролем проходит.
	resp = postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"password12"}, "new2": {"password12"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set status = %d, want 200", resp.StatusCode)
	}
	if _, err := s.auth.Authenticate(ctx, "setpw@example.com", "password12"); err != nil {
		t.Fatalf("Authenticate after set: %v", err)
	}
}
