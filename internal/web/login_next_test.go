package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

func TestLoginReturnsToRequestedPage(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, _ = orgSettingsRegister(t, authSvc, "loginnext@example.com")

	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/profile", nil)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET /profile: %v", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("Location = %q, want /login?next=...", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("/profile")) {
		t.Fatalf("Location = %q, адресат не сохранён", loc)
	}

	page, err := http.Get(s.srv.URL + loc)
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `name="next" value="/profile"`) {
		t.Fatalf("форма входа не сохранила адресата:\n%s", body)
	}

	resp = postForm(t, s.srv, "/login", url.Values{
		"email": {"loginnext@example.com"}, "password": {"correct-horse-battery"},
		"next": {"/profile"},
	}, s.srv.URL, nil)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/profile" {
		t.Fatalf("после входа Location = %q, want /profile", got)
	}
}

func TestLoginRejectsForeignNext(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, _ = orgSettingsRegister(t, authSvc, "loginnext2@example.com")

	// «/\» браузер нормализует в «//», то есть в протокол-относительный адрес
	// — это переход на чужой сайт, и он должен отбрасываться наравне с «//».
	for _, bad := range []string{
		"https://evil.example/", "//evil.example/", `/\evil.example/`, "javascript:alert(1)",
	} {
		resp := postForm(t, s.srv, "/login", url.Values{
			"email": {"loginnext2@example.com"}, "password": {"correct-horse-battery"},
			"next": {bad},
		}, s.srv.URL, nil)
		resp.Body.Close()
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("next=%q увёл на %q, want / ", bad, got)
		}
	}
}

func TestRequireUserDoesNotSaveNextForPOST(t *testing.T) {
	s := newStack(t)

	resp := postForm(t, s.srv, "/projects/1/alerts/rules", url.Values{"x": {"1"}}, s.srv.URL, nil)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("POST без сессии → %q, want голый /login", got)
	}
}
