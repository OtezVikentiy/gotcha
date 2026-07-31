package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRegisterCarriesNext — форма регистрации сохраняет адресата так же, как
// форма входа. Без этого пришедший по ссылке-приглашению после регистрации
// оказывается на главной, а приглашение висит непринятым — именно эту дыру в
// потоке когда-то закрыли обходом по email.
func TestRegisterCarriesNext(t *testing.T) {
	s := newStack(t)

	resp, err := http.Get(s.srv.URL + "/register?next=" + url.QueryEscape("/invite/tok123"))
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="next" value="/invite/tok123"`) {
		t.Fatalf("форма регистрации не сохранила адресата:\n%s", body)
	}
}

// TestRegisterRedirectsToNext — после успешной регистрации человек попадает
// туда, куда шёл.
func TestRegisterRedirectsToNext(t *testing.T) {
	s := newStack(t) // режим open: гейт приглашений сюда не вмешивается
	resp := postForm(t, s.srv, "/register", url.Values{
		"email": {"regnext@example.com"}, "password": {"correct-horse-battery"},
		"password2": {"correct-horse-battery"}, "next": {"/invite/tok123"},
	}, s.srv.URL, nil)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/invite/tok123" {
		t.Fatalf("после регистрации Location = %q, want /invite/tok123", got)
	}
}

// TestRegisterIgnoresForeignNext — чужой адрес в next не должен уводить с
// сайта; та же защита, что у входа (safeNextPath).
func TestRegisterIgnoresForeignNext(t *testing.T) {
	s := newStack(t)
	for _, next := range []string{"https://evil.example/", "//evil.example/", `/\evil.example/`} {
		resp := postForm(t, s.srv, "/register", url.Values{
			"email":     {"regforeign" + url.QueryEscape(next) + "@example.com"},
			"password":  {"correct-horse-battery"},
			"password2": {"correct-horse-battery"},
			"next":      {next},
		}, s.srv.URL, nil)
		resp.Body.Close()
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("next=%q → Location = %q, want /", next, got)
		}
	}
}

// TestRegisterClosedLinksToLoginWithNext — раунд правок 1: в закрытой ветке
// (registrationClosed) формы нет вовсе, но ссылка «уже есть аккаунт» на
// /login обязана нести next дальше — иначе адресат из ссылки-приглашения
// теряется ровно там же, где раньше терялся при отказе (denyRegistration).
func TestRegisterClosedLinksToLoginWithNext(t *testing.T) {
	s := newStack(t)
	s.h.RegistrationMode = "closed"

	// Bootstrap первого пользователя: registrationClosed прячет форму только
	// когда UserCount > 0 (первый всегда может зарегистрироваться).
	resp := postForm(t, s.srv, "/register", regForm("closed-bootstrap@example.com"), s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap register status = %d, want 303", resp.StatusCode)
	}
	if n, err := s.h.Auth.UserCount(context.Background()); err != nil || n != 1 {
		t.Fatalf("UserCount = %d, err = %v, want 1", n, err)
	}

	getResp, err := http.Get(s.srv.URL + "/register?next=" + url.QueryEscape("/invite/tok123"))
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !strings.Contains(string(body), `href="/login?next=`+url.QueryEscape("/invite/tok123")+`"`) {
		t.Fatalf("ссылка на /login в закрытой ветке не сохранила адресата:\n%s", body)
	}
}
