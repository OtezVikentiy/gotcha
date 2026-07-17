package web

import (
	"context"
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

	// sso-{id} строит OIDC с метаданными.
	p, meta, ok := h.resolveProvider(ctx, "sso-"+itoa(orgID))
	if !ok || meta == nil || meta.OrgID != orgID || meta.Domain != "corp.com" || p.Name() != "oidc" {
		t.Fatalf("resolve sso = (%v, %+v, %v)", p, meta, ok)
	}
	// Несуществующий орг → false.
	if _, _, ok := h.resolveProvider(ctx, "sso-999999"); ok {
		t.Fatal("resolve sso-999999 must be false")
	}
	// Обычное имя без Registry → false.
	if _, _, ok := h.resolveProvider(ctx, "oidc"); ok {
		t.Fatal("resolve oidc without registry must be false")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestEmailDomain — emailDomain нормализует регистр/пробелы И обрезает конечную
// точку FQDN (RA-L2): "user@enforced.com." эквивалентен "enforced.com", иначе
// trailing-dot обходил бы enforced-SSO гейт/domain guard.
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

	call := func(id oauth.Identity) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/cb", nil)
		h.ssoCallback(w, r, name, id, meta)
		return w
	}

	// Новый юзер из домена → создан, член орга, identity, сессия, 303 /.
	w := call(oauth.Identity{Subject: "sub-1", Email: "alice@corp.com", EmailVerified: true})
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

	// Повторный вход по субъекту → сессия, без дублей.
	w = call(oauth.Identity{Subject: "sub-1", Email: "alice@corp.com", EmailVerified: true})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("subject re-login code = %d", w.Code)
	}

	// Чужой домен → 403, юзер не создан.
	w = call(oauth.Identity{Subject: "sub-2", Email: "bob@evil.com", EmailVerified: true})
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign domain code = %d, want 403", w.Code)
	}
	if _, err := authSvc.UserByEmail(ctx, "bob@evil.com"); err == nil {
		t.Fatal("foreign-domain user must not be created")
	}

	// Не-verified → 403.
	w = call(oauth.Identity{Subject: "sub-3", Email: "carol@corp.com", EmailVerified: false})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified code = %d, want 403", w.Code)
	}

	// Существующий (password) юзер того же домена → линкуется + член орга.
	existing, _ := authSvc.Register(ctx, "dave@corp.com", "password12")
	w = call(oauth.Identity{Subject: "sub-4", Email: "dave@corp.com", EmailVerified: true})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("existing user code = %d", w.Code)
	}
	if role, err := orgSvc.Role(ctx, orgID, existing); err != nil || role != org.RoleMember {
		t.Fatalf("existing user not made member: role=%v err=%v", role, err)
	}
	if got, _ := authSvc.IdentityUser(ctx, name, "sub-4"); got != existing {
		t.Fatal("existing user identity not linked")
	}
}
