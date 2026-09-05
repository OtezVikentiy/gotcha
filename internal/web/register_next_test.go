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
// /login обязана нести next дальше — иначе адресат из глубокой ссылки
// теряется ровно там же, где раньше терялся при отказе (denyRegistration).
//
// Пример — некий защищённый путь приложения, а не /invite/{token}: последний
// с K9-19 обзавёлся особым случаем ниже (TestRegisterClosedInvitePathAvoidsQuery)
// именно потому, что путь приглашения в query кладётся отдельно от обычного
// next и теперь не кладётся вовсе.
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

	getResp, err := http.Get(s.srv.URL + "/register?next=" + url.QueryEscape("/profile"))
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !strings.Contains(string(body), `href="/login?next=`+url.QueryEscape("/profile")+`"`) {
		t.Fatalf("ссылка на /login в закрытой ветке не сохранила адресата:\n%s", body)
	}
}

// TestRegisterClosedInvitePathAvoidsQuery — K9-19: тот же сценарий (закрытая
// ветка регистрации без формы, ссылка «уже есть аккаунт»), но next — путь
// приглашения. Раньше ссылка несла его в query (next=/invite/{token}) —
// ровно та находка, которую чинит T7. Токен по-прежнему не теряется: он
// остаётся годным для восстановления адресата, но едет invite-cookie, а не
// адресом — здесь это легаси-случай (next в query пришёл как есть, старой
// ссылкой или руками), и resolveAuthNext зеркалит его в cookie на лету (см.
// auth.go).
func TestRegisterClosedInvitePathAvoidsQuery(t *testing.T) {
	s := newStack(t)
	s.h.RegistrationMode = "closed"

	resp := postForm(t, s.srv, "/register", regForm("closed-bootstrap2@example.com"), s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap register status = %d, want 303", resp.StatusCode)
	}

	getResp, err := http.Get(s.srv.URL + "/register?next=" + url.QueryEscape("/invite/tok123"))
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	page := string(body)

	if strings.Contains(page, url.QueryEscape("tok123")) {
		t.Errorf("токен приглашения не должен встречаться в query ссылки «войти»:\n%s", page)
	}
	if !strings.Contains(page, `href="/login"`) {
		t.Errorf("ссылка «войти» должна быть голой (без next в query):\n%s", page)
	}
	var inviteCookie *http.Cookie
	for _, c := range getResp.Cookies() {
		if c.Name == "invite_next" {
			inviteCookie = c
		}
	}
	if inviteCookie == nil || inviteCookie.Value != "tok123" {
		t.Fatal("токен приглашения не зеркалится в invite-cookie — адресат потерян")
	}
}
