package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type oauthProfileStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	auth *auth.Service
	org  *org.Service
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
	return &oauthProfileStack{pool: pool, srv: srv, auth: authSvc, org: orgSvc}
}

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
	if !strings.Contains(bs, "Отвязать") {
		t.Fatalf("profile missing unlink button: %s", bs)
	}
	if !strings.Contains(bs, "/auth/oauth/yandex/start?link=1") {
		t.Fatalf("profile missing linkable yandex: %s", bs)
	}
}

// Инстанс без единого настроенного провайдера не должен обещать привязку — раньше
// пустое состояние звало «привяжите GitHub, GitLab», которых в продукте нет вовсе.
func TestProfileNoProviderConfiguredShowsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s := newOAuthProfileStack(t)
	ctx := context.Background()
	uid, err := s.auth.Register(ctx, "prof-noauth@example.com", "password12")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	resp := getWithCookie(t, s.srv, "/profile", cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bs := string(body)
	if strings.Contains(bs, "GitHub") || strings.Contains(bs, "GitLab") {
		t.Errorf("пустое состояние обещает несуществующих провайдеров: %s", bs)
	}
	if !strings.Contains(bs, "Вход через провайдера недоступен") {
		t.Errorf("нет пояснения о недоступности входа через провайдера: %s", bs)
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

	resp := postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlink last method status = %d, want 409", resp.StatusCode)
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-1"); err != nil {
		t.Fatalf("identity must remain after blocked unlink: %v", err)
	}

	if err := s.auth.SetPassword(ctx, uid, "newpassword12"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	// Без confirmed=yes — страница подтверждения, отвязки ещё не произошло.
	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}}, s.srv.URL, cookie)
	confirmBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(confirmBody), "OIDC") {
		t.Fatalf("подтверждение отвязки не называет провайдера: status=%d, %s", resp.StatusCode, confirmBody)
	}
	if _, err := s.auth.IdentityUser(ctx, "oidc", "sub-1"); err != nil {
		t.Fatalf("identity must remain before confirmation: %v", err)
	}

	resp = postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"oidc"}, "confirmed": {"yes"}}, s.srv.URL, cookie)
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

	resp := postForm(t, s.srv, "/profile/password/set", url.Values{"new": {"password12"}, "new2": {"different12"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch status = %d, want 422", resp.StatusCode)
	}
	if has, _ := s.auth.HasPassword(ctx, uid); has {
		t.Fatal("password must not be set on mismatch")
	}
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

// Единый вход резолвится по префиксу "sso-" и не сводится к h.OAuth.List() — без этой
// кнопки владелец парольного аккаунта после включения SSO в организации упирался в
// сообщение «привяжите в профиле», за которым ничего не стояло (см. error.oauth.sso_account_exists).
func TestProfileShowsSSOLinkable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s := newOAuthProfileStack(t)
	ctx := context.Background()
	uid, err := s.auth.Register(ctx, "member@corp.com", "password12")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	o, err := s.org.CreateOrg(ctx, "ssoprof", "SSO Prof Co", uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.UpsertSSO(ctx, org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s",
		Domain: "corp.com", DefaultRole: "member", Enforced: false,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	// Вторая организация того же пользователя БЕЗ настроенного SSO — не должна предлагать привязку.
	noSSO, err := s.org.CreateOrg(ctx, "nossoprof", "No SSO Co", uid)
	if err != nil {
		t.Fatalf("create no-sso org: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	resp := getWithCookie(t, s.srv, "/profile", cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bs := string(body)
	wantHref := "/auth/oauth/sso-" + strconv.FormatInt(o.ID, 10) + "/start?link=1"
	if !strings.Contains(bs, wantHref) {
		t.Fatalf("profile missing SSO link href %q: %s", wantHref, bs)
	}
	if !strings.Contains(bs, "SSO Prof Co") {
		t.Fatalf("profile missing org name in SSO link label: %s", bs)
	}
	noSSOHref := "/auth/oauth/sso-" + strconv.FormatInt(noSSO.ID, 10) + "/start?link=1"
	if strings.Contains(bs, noSSOHref) {
		t.Fatalf("profile offers to link SSO for an org that has none configured: %s", bs)
	}

	// Уже привязанная SSO-идентичность не предлагается для повторной привязки.
	if err := s.auth.LinkIdentity(ctx, uid, "sso-"+strconv.FormatInt(o.ID, 10), "sub-1", "member@corp.com"); err != nil {
		t.Fatalf("link: %v", err)
	}
	resp = getWithCookie(t, s.srv, "/profile", cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	bs = string(body)
	if strings.Contains(bs, wantHref) {
		t.Fatalf("profile still offers to link an already-linked SSO identity: %s", bs)
	}
}
