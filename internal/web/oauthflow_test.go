package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// staticProvider — тестовый провайдер с предсказуемым AuthURL.
type staticProvider struct{ name, display, authBase string }

func (s staticProvider) Name() string { return s.name }
func (s staticProvider) DisplayName() string {
	if s.display != "" {
		return s.display
	}
	return s.name
}
func (s staticProvider) AuthURL(state, nonce, chal, redir string) string {
	return s.authBase + "?state=" + state + "&redir=" + redir
}
func (s staticProvider) Exchange(_ context.Context, _, _, _, _ string) (oauth.Identity, error) {
	return oauth.Identity{}, nil
}

func TestOAuthStartSetsCookieAndRedirects(t *testing.T) {
	h := web.New(nil, nil, nil, nil, "http://localhost:8080")
	h.OAuth = oauth.NewRegistry(staticProvider{name: "oidc", authBase: "https://idp/authorize"})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/auth/oauth/oidc/start")
	if err != nil {
		t.Fatalf("GET start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://idp/authorize?state=") {
		t.Fatalf("Location = %q", loc)
	}
	var found bool
	for _, ck := range resp.Cookies() {
		if ck.Name == "gotcha_oauth" && ck.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("flow cookie not set")
	}
	resp2, _ := c.Get(srv.URL + "/auth/oauth/unknown/start")
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestLoginPageShowsProviderButtons(t *testing.T) {
	h := web.New(nil, nil, nil, nil, "http://localhost:8080")
	h.OAuth = oauth.NewRegistry(staticProvider{name: "yandex", display: "Яндекс", authBase: "https://ya/authorize"})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/auth/oauth/yandex/start") {
		t.Fatalf("login page missing provider button: %s", body)
	}
}
