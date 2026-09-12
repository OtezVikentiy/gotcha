package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

func TestFormBodyOverGeneralLimitReturns413(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "formlimit-413@example.com")

	huge := strings.Repeat("x", 70_000) // больше formBodyMaxBytes (64 КиБ), меньше implicit 10 МиБ stdlib
	resp := postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {huge}}, s.srv.URL, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "превышает допустимый размер") {
		t.Fatalf("body missing localized 413 message (error.body_too_large): %s", body)
	}
}

func TestFormBodyWithinGeneralLimitParsesNormally(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "formlimit-ok@example.com")

	resp := postForm(t, s.srv, "/profile/identities/unlink", url.Values{"provider": {"nonexistent"}}, s.srv.URL, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (нет такой привязки, тело разобралось штатно): %s", resp.StatusCode, body)
	}
}

func TestFormBodyGeneralLimitDoesNotOverrideStricterAuthLimit(t *testing.T) {
	s := newStack(t)

	// 20 КиБ — между authFormMaxBodyBytes (8 КиБ) и formBodyMaxBytes (64 КиБ): если бы общий
	// предел затирал частный, это тело прошло бы ParseForm без ошибки.
	body := "email=" + strings.Repeat("a", 20*1024) + "@x.com&password=x"
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+"/login", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.srv.URL)

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (частный auth-предел 8 КиБ обязан сработать раньше общего 64 КиБ): %s", resp.StatusCode, respBody)
	}
}
