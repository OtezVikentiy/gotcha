package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

// TestLoginReturnsToRequestedPage — глубокая ссылка переживает форму входа.
//
// Раньше требование авторизации отправляло на голый /login, и адресат
// терялся: пришедший по глубокой ссылке (например, на профиль или на
// проблему из письма алерта) входил и оказывался на главной.
//
// Пример — GET /profile, а не /invite/{token}: последний с задачи 8 сам
// сделан публичным (аноним должен УВИДЕТЬ приглашение, а не улететь на
// /login, потеряв токен), requireUser его больше не оборачивает — см. web.go.
func TestLoginReturnsToRequestedPage(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, _ = orgSettingsRegister(t, authSvc, "loginnext@example.com")

	// Неавторизованный запрос глубокой ссылки уводит на форму входа, сохраняя,
	// куда человек шёл.
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

	// Форма входа несёт его скрытым полем.
	page, err := http.Get(s.srv.URL + loc)
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `name="next" value="/profile"`) {
		t.Fatalf("форма входа не сохранила адресата:\n%s", body)
	}

	// И вход возвращает именно туда.
	resp = postForm(t, s.srv, "/login", url.Values{
		"email": {"loginnext@example.com"}, "password": {"correct-horse-battery"},
		"next": {"/profile"},
	}, s.srv.URL, nil)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/profile" {
		t.Fatalf("после входа Location = %q, want /profile", got)
	}
}

// TestLoginRejectsForeignNext — адресат приходит из формы, то есть от клиента:
// увести им на чужой сайт нельзя.
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

// TestRequireUserDoesNotSaveNextForPOST — тело POST после входа не
// восстановить, а повторять его молча означало бы выполнить действие, которого
// человек в этот раз не просил.
func TestRequireUserDoesNotSaveNextForPOST(t *testing.T) {
	s := newStack(t)

	resp := postForm(t, s.srv, "/projects/1/alerts/rules", url.Values{"x": {"1"}}, s.srv.URL, nil)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("POST без сессии → %q, want голый /login", got)
	}
}
