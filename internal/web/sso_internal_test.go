package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func ssoTestHandler(t *testing.T) (*Handler, *org.Service, *auth.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := New(authSvc, orgSvc, nil, nil, "http://localhost:8080")
	return h, orgSvc, authSvc, pool
}

func mkSSOOrg(t *testing.T, pool *pgxpool.Pool, orgSvc *org.Service, slug, domain string, enforced bool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, "INSERT INTO users (email,password_hash) VALUES ($1,'x') RETURNING id", slug+"-o@x.com").Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := orgSvc.CreateOrg(ctx, slug, slug, uid)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := orgSvc.UpsertSSO(ctx, org.SSOConfig{
		OrgID: o.ID, Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s",
		Domain: domain, DefaultRole: "member", Enforced: enforced,
	}); err != nil {
		t.Fatalf("upsert sso: %v", err)
	}
	return o.ID
}

func TestResolveProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	h, orgSvc, _, pool := ssoTestHandler(t)
	orgID := mkSSOOrg(t, pool, orgSvc, "rp", "corp.com", false)
	ctx := context.Background()

	p, meta, ok := h.resolveProvider(ctx, "sso-"+itoa(orgID))
	if !ok || meta == nil || meta.OrgID != orgID || meta.Domain != "corp.com" || p.Name() != "oidc" {
		t.Fatalf("resolve sso = (%v, %+v, %v)", p, meta, ok)
	}
	if _, _, ok := h.resolveProvider(ctx, "sso-999999"); ok {
		t.Fatal("resolve sso-999999 must be false")
	}
	if _, _, ok := h.resolveProvider(ctx, "oidc"); ok {
		t.Fatal("resolve oidc without registry must be false")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// trailing-dot эквивалентен домену без точки, иначе он обходил бы enforced-SSO гейт/domain guard.
func TestEmailDomain(t *testing.T) {
	cases := map[string]string{
		"user@x.com":      "x.com",
		"user@x.com.":     "x.com",
		"USER@X.COM.":     "x.com",
		"  user@x.com.  ": "x.com",
		"user@sub.x.com.": "sub.x.com",
		"nodomain":        "",
		"trailing@":       "",
		"user@x.com..":    "x.com",
	}
	for in, want := range cases {
		if got := emailDomain(in); got != want {
			t.Errorf("emailDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSOCallbackJIT(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	h, orgSvc, authSvc, pool := ssoTestHandler(t)
	orgID := mkSSOOrg(t, pool, orgSvc, "jit", "corp.com", false)
	name := "sso-" + itoa(orgID)
	meta := &ssoMeta{OrgID: orgID, Domain: "corp.com", DefaultRole: "member"}
	ctx := context.Background()

	call := func(id oauth.Identity, flow oauthFlow) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/cb", nil)
		h.ssoCallback(w, r, name, id, meta, flow)
		return w
	}

	w := call(oauth.Identity{Subject: "sub-1", Email: "alice@corp.com", EmailVerified: true}, oauthFlow{})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("new user code/loc = %d/%s", w.Code, w.Header().Get("Location"))
	}
	uid, err := authSvc.UserByEmail(ctx, "alice@corp.com")
	if err != nil {
		t.Fatalf("provisioned user missing: %v", err)
	}
	if role, err := orgSvc.Role(ctx, orgID, uid); err != nil || role != org.RoleMember {
		t.Fatalf("member role = %v err=%v", role, err)
	}
	if got, _ := authSvc.IdentityUser(ctx, name, "sub-1"); got != uid {
		t.Fatalf("identity not linked")
	}
	hasSession := false
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatal("no session cookie for new SSO user")
	}

	w = call(oauth.Identity{Subject: "sub-1", Email: "alice@corp.com", EmailVerified: true}, oauthFlow{})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("subject re-login code = %d", w.Code)
	}

	w = call(oauth.Identity{Subject: "sub-2", Email: "bob@evil.com", EmailVerified: true}, oauthFlow{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign domain code = %d, want 403", w.Code)
	}
	if _, err := authSvc.UserByEmail(ctx, "bob@evil.com"); err == nil {
		t.Fatal("foreign-domain user must not be created")
	}

	w = call(oauth.Identity{Subject: "sub-3", Email: "carol@corp.com", EmailVerified: false}, oauthFlow{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified code = %d, want 403", w.Code)
	}

	// Существующий парольный аккаунт: чужой IdP с тем же доменом не должен уметь войти
	// как этот пользователь по одному лишь совпадению email — только явной привязкой ниже.
	existing, _ := authSvc.Register(ctx, "dave@corp.com", "password12")
	w = call(oauth.Identity{Subject: "sub-4", Email: "dave@corp.com", EmailVerified: true}, oauthFlow{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("existing account without session code = %d, want 403", w.Code)
	}
	if _, err := authSvc.IdentityUser(ctx, name, "sub-4"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("existing account must not be silently linked: err=%v", err)
	}
	if _, err := orgSvc.Role(ctx, orgID, existing); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("existing account must not gain membership without consent: err=%v", err)
	}

	// Привязка с активной сессией владельца аккаунта — единственный путь, каким этот
	// же email завершает линковку и получает членство.
	token, err := authSvc.CreateSession(ctx, existing)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/cb", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "sub-4", Email: "dave@corp.com", EmailVerified: true}, meta,
		oauthFlow{Link: true, UID: existing})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/profile" {
		t.Fatalf("session-confirmed link code/loc = %d/%s", w.Code, w.Header().Get("Location"))
	}
	if got, err := authSvc.IdentityUser(ctx, name, "sub-4"); err != nil || got != existing {
		t.Fatalf("session-confirmed link missing: got=%v err=%v", got, err)
	}
	if role, err := orgSvc.Role(ctx, orgID, existing); err != nil || role != org.RoleMember {
		t.Fatalf("session-confirmed link must grant membership: role=%v err=%v", role, err)
	}

	// UID из flow обязан совпасть с сессией — иначе чужая привязка запросом с чужим
	// flow.UID (например, перехваченным state) линковала бы аккаунт не той сессии.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/cb", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "sub-5", Email: "dave@corp.com", EmailVerified: true}, meta,
		oauthFlow{Link: true, UID: existing + 1})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched flow.UID code = %d, want 400", w.Code)
	}
	if _, err := authSvc.IdentityUser(ctx, name, "sub-5"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("mismatched flow.UID must not link: err=%v", err)
	}
}

// Покрывает четыре ветки flow.Link, не задетые TestSSOCallbackJIT: отсутствие сессии,
// конфликт занятой идентичности, прочую ошибку LinkIdentity и отказ EnsureMember.
func TestSSOCallbackLinkErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	h, orgSvc, authSvc, pool := ssoTestHandler(t)
	orgID := mkSSOOrg(t, pool, orgSvc, "linkerr", "corp.com", false)
	name := "sso-" + itoa(orgID)
	meta := &ssoMeta{OrgID: orgID, Domain: "corp.com", DefaultRole: "member"}
	ctx := context.Background()

	existing, err := authSvc.Register(ctx, "erin@corp.com", "password12")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Без активной сессии на колбэке flow.Link уводит на /login, а не линкует вслепую.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/cb", nil)
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "no-session", Email: "erin@corp.com", EmailVerified: true}, meta,
		oauthFlow{Link: true, UID: existing})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("no session code/loc = %d/%s, want 303 /login", w.Code, w.Header().Get("Location"))
	}
	if _, err := authSvc.IdentityUser(ctx, name, "no-session"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("no session must not link: err=%v", err)
	}

	// Конфликт занятой идентичности: тот же provider+subject уже привязан к ДРУГОМУ пользователю.
	other, err := authSvc.Register(ctx, "frank@corp.com", "password12")
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	if err := authSvc.LinkIdentity(ctx, other, name, "taken-subject", "frank@corp.com"); err != nil {
		t.Fatalf("pre-link: %v", err)
	}
	token, err := authSvc.CreateSession(ctx, existing)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/cb", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "taken-subject", Email: "erin@corp.com", EmailVerified: true}, meta,
		oauthFlow{Link: true, UID: existing})
	if w.Code != http.StatusConflict {
		t.Fatalf("identity taken code = %d, want 409", w.Code)
	}
	if got, _ := authSvc.IdentityUser(ctx, name, "taken-subject"); got != other {
		t.Fatalf("identity taken must not move to a new owner: got=%v", got)
	}

	// Прочая ошибка привязки (не конфликт уникальности): встроенный NUL в email валит
	// INSERT на уровне БД иначе, чем 23505 (subject свежий, чтобы не упасть раньше на
	// самом SELECT в IdentityUser, который email вообще не читает), — должен уйти
	// в default-ветку, не в конфликт.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/cb", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "generic-link-error", Email: "eri\x00n@corp.com", EmailVerified: true}, meta,
		oauthFlow{Link: true, UID: existing})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("generic link error code = %d, want 500", w.Code)
	}
	if _, err := authSvc.IdentityUser(ctx, name, "generic-link-error"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("failed generic link must not leave a partial identity: err=%v", err)
	}

	// Ошибка добавления в участники: организация из meta не существует — EnsureMember обязан
	// отказать после успешной привязки идентичности, а не пропустить членство молча.
	bogusMeta := &ssoMeta{OrgID: 99_999_999, Domain: "corp.com", DefaultRole: "member"}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/cb", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ssoCallback(w, r, name, oauth.Identity{Subject: "ensuremember-fail", Email: "erin@corp.com", EmailVerified: true}, bogusMeta,
		oauthFlow{Link: true, UID: existing})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("ensure member error code = %d, want 500", w.Code)
	}
	if _, err := orgSvc.Role(ctx, orgID, existing); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("failed EnsureMember must not grant membership in the real org: err=%v", err)
	}
}
