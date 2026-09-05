package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

// TestFormBodyOverGeneralLimitReturns413 — K7-4: тело обычной формы продукта
// сверх общего предела (formBodyMaxBytes, 64 КиБ) отвечает 413 через ту же
// локализованную страницу ошибки, что и остальные отказы веб-слоя
// (h.renderError), а не 500 и не молчаливым усечением ParseForm до
// implicit-дефолта stdlib (10 МиБ). /profile/identities/unlink — обычный
// requireUser-хендлер без своего частного предела (в отличие от
// auth/heartbeat/probe) — именно на таких раньше предел не был поставлен
// нигде явно.
func TestFormBodyOverGeneralLimitReturns413(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "formlimit-413@example.com")

	huge := strings.Repeat("x", 70_000) // > formBodyMaxBytes (64 КиБ), << implicit 10 МиБ stdlib
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

// TestFormBodyWithinGeneralLimitParsesNormally — тело в пределах общего
// лимита доходит до обработчика как обычно: ParseForm не режет поле и не
// молчаливо его усекает. Пустой/несуществующий provider не находит привязки
// → 422 (см. profileIdentityUnlink), а не 413/400 — регресс на «предел
// слишком тесен для обычной формы».
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

// TestFormBodyGeneralLimitDoesNotOverrideStricterAuthLimit — K7-4: общий
// предел (64 КиБ) обязан КОМПОНОВАТЬСЯ с более строгим частным пределом
// логина (authFormMaxBodyBytes, 8 КиБ), а не заменять его собой — тело между
// 8 КиБ и 64 КиБ обязано по-прежнему отбиваться частным пределом.
// requireUser здесь не участвует (страница логина публичная), так что
// проверка идёт через тот же путь HTTP, что и настоящий клиент.
func TestFormBodyGeneralLimitDoesNotOverrideStricterAuthLimit(t *testing.T) {
	s := newStack(t)

	// 20 КиБ — больше authFormMaxBodyBytes (8 КиБ), меньше formBodyMaxBytes
	// (64 КиБ). Если бы общий предел затирал частный, это тело прошло бы
	// ParseForm без ошибки.
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
