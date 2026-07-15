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

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type ssoWebStack struct {
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
}

func newSSOWebStack(t *testing.T) *ssoWebStack {
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
	h.Register(mux)
	return &ssoWebStack{srv: srv, org: orgSvc, auth: authSvc}
}

func TestLoginEnforcementAndSSO(t *testing.T) {
	s := newSSOWebStack(t)
	ctx := context.Background()

	// enforced-SSO орг для corp.com.
	ownerID, _ := orgSettingsRegister(t, s.auth, "sso-owner@corp.com")
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
	// Юзер с corp.com-паролем (существовал до enforced).
	if _, err := s.auth.Register(ctx, "worker@corp.com", "password12"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Принуждение: POST /login с corp.com email → 422, пароль не проверяется.
	form := url.Values{"email": {"worker@corp.com"}, "password": {"password12"}}
	resp := postForm(t, s.srv, "/login", form, s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "требует вход через SSO") {
		t.Fatalf("enforced login status=%d body=%s", resp.StatusCode, body)
	}

	// POST /sso с corp.com email → 303 на sso-start орга.
	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"anyone@corp.com"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sso submit status = %d, want 303", resp.StatusCode)
	}
	wantLoc := "/auth/oauth/sso-" + strconv.FormatInt(o.ID, 10) + "/start"
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Fatalf("sso redirect = %q, want %q", got, wantLoc)
	}

	// POST /sso с неизвестным доменом → 422.
	resp = postForm(t, s.srv, "/sso", url.Values{"email": {"x@unknown.com"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown-domain sso status = %d, want 422", resp.StatusCode)
	}

	// Не-enforced SSO-домен: пароль работает как обычно.
	owner2, _ := orgSettingsRegister(t, s.auth, "opt-owner@opt.com")
	o2, _ := s.org.CreateOrg(ctx, "opt-co", "Opt Co", owner2)
	s.org.UpsertSSO(ctx, org.SSOConfig{OrgID: o2.ID, Issuer: "https://i", ClientID: "c", ClientSecret: "s", Domain: "opt.com", DefaultRole: "member", Enforced: false})
	if _, err := s.auth.Register(ctx, "user@opt.com", "password12"); err != nil {
		t.Fatalf("register opt: %v", err)
	}
	resp = postForm(t, s.srv, "/login", url.Values{"email": {"user@opt.com"}, "password": {"password12"}}, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("non-enforced password login status = %d, want 303", resp.StatusCode)
	}
}
