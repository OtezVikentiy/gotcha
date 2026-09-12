package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

func TestInvitePageGuidesAnonymous(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "guest@corp.example", "member")

	resp, err := http.Get(s.srv.URL + "/invite/" + token)
	if err != nil {
		t.Fatalf("GET /invite: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)

	// seedOrgWithInvite заводит организацию с фиксированным названием (slug уникален через
	// seedSeq, название — нет).
	if !strings.Contains(page, "Seed Co") {
		t.Error("страница не называет организацию — человек подтверждает вслепую")
	}
	// Адрес приглашения не показывается: страница публична, токен может утечь. Гейт
	// регистрации требует токен И совпадение адреса — показ здесь свёл бы это к одному фактору.
	if strings.Contains(page, "guest@corp.example") {
		t.Error("страница не должна называть адрес приглашения — держатель токена не обязан быть приглашённым")
	}
	// Роль — человеческой подписью (memberRoleLabelKey), не сырым значением "member".
	if !strings.Contains(page, "Участник") {
		t.Error("роль должна выводиться человеческой подписью, а не сырым значением")
	}
	if strings.Contains(page, url.QueryEscape(token)) {
		t.Error("токен приглашения не должен встречаться в query — он был найден на странице закодированным")
	}
	if !strings.Contains(page, `href="/register"`) {
		t.Error("нет голой ссылки на регистрацию (без next в query)")
	}
	if !strings.Contains(page, `href="/login"`) {
		t.Error("нет голой ссылки на вход (без next в query)")
	}
	// Адресат переживает переход через invite-cookie, HttpOnly, а не через query.
	var inviteCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			inviteCookie = c
		}
	}
	if inviteCookie == nil {
		t.Fatal("invite-cookie не выставлена анониму — адресат после /login и /register будет потерян")
	}
	if inviteCookie.Value != token {
		t.Errorf("invite-cookie несёт %q, want token %q", inviteCookie.Value, token)
	}
	if !inviteCookie.HttpOnly {
		t.Error("invite-cookie обязана быть HttpOnly — иначе токен читается из JS")
	}
}

func TestInvitePageAnonymousLoginRoundTrip(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "roundtrip@corp.example", "member")
	inviteAcceptPath := "/invite/" + token

	if _, err := s.auth.Register(t.Context(), "roundtrip@corp.example", "correct-horse-battery"); err != nil {
		t.Fatalf("register: %v", err)
	}

	getInvite, err := http.Get(s.srv.URL + inviteAcceptPath)
	if err != nil {
		t.Fatalf("GET %s: %v", inviteAcceptPath, err)
	}
	io.Copy(io.Discard, getInvite.Body)
	getInvite.Body.Close()
	var inviteCookie *http.Cookie
	for _, c := range getInvite.Cookies() {
		if c.Name == "invite_next" {
			inviteCookie = c
		}
	}
	if inviteCookie == nil {
		t.Fatal("invite-cookie не выставлена")
	}

	loginReq, err := http.NewRequest(http.MethodGet, s.srv.URL+"/login", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	loginReq.AddCookie(inviteCookie)
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if !strings.Contains(string(loginBody), `name="next" value="`+inviteAcceptPath+`"`) {
		t.Fatalf("форма входа не восстановила адресата из invite-cookie:\n%s", loginBody)
	}

	resp := postForm(t, s.srv, "/login", url.Values{
		"email": {"roundtrip@corp.example"}, "password": {"correct-horse-battery"},
		"next": {inviteAcceptPath},
	}, s.srv.URL, inviteCookie)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != inviteAcceptPath {
		t.Fatalf("после входа Location = %q, want %q", got, inviteAcceptPath)
	}
	if strings.Contains(resp.Header.Get("Location"), "?") {
		t.Errorf("Location несёт query: %q", resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" && c.Value != "" {
			t.Error("invite-cookie должна быть погашена после успешного входа")
		}
	}
}

func TestInvitePageHidesDeadToken(t *testing.T) {
	s := newInviteModeStack(t)
	resp, err := http.Get(s.srv.URL + "/invite/no-such-token")
	if err != nil {
		t.Fatalf("GET /invite: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("мёртвый токен = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "риглашение недействительно") {
		t.Error("мёртвый токен должен показать err.org.invite_invalid, тот же текст, что и у POST")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			t.Error("invite-cookie не должна выставляться для несуществующего/просроченного токена")
		}
	}
}

func TestInvitePageAuthenticatedShowsAcceptForm(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "member-to-be@corp.example", "member")

	uid, err := s.auth.Register(t.Context(), "member-to-be@corp.example", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	cookie := loginCookie(t, s.auth, uid)

	req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/invite/"+token, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /invite: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)

	if !strings.Contains(page, `action="/invite/`+token+`"`) {
		t.Error("авторизованный держатель токена должен видеть форму принятия")
	}
	if strings.Contains(page, "/register?next=") {
		t.Error("авторизованному не нужна ссылка на регистрацию")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			t.Error("авторизованному invite-cookie не нужна")
		}
	}
}

func TestInvitePageAnonymousRegisterRoundTrip(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "newbie@corp.example", "member")
	inviteAcceptPath := "/invite/" + token

	getInvite, err := http.Get(s.srv.URL + inviteAcceptPath)
	if err != nil {
		t.Fatalf("GET %s: %v", inviteAcceptPath, err)
	}
	io.Copy(io.Discard, getInvite.Body)
	getInvite.Body.Close()
	var inviteCookie *http.Cookie
	for _, c := range getInvite.Cookies() {
		if c.Name == "invite_next" {
			inviteCookie = c
		}
	}
	if inviteCookie == nil {
		t.Fatal("invite-cookie не выставлена")
	}

	regReq, err := http.NewRequest(http.MethodGet, s.srv.URL+"/register", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	regReq.AddCookie(inviteCookie)
	regResp, err := http.DefaultClient.Do(regReq)
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	regBody, _ := io.ReadAll(regResp.Body)
	regResp.Body.Close()
	if !strings.Contains(string(regBody), `name="next" value="`+inviteAcceptPath+`"`) {
		t.Fatalf("форма регистрации не восстановила адресата из invite-cookie:\n%s", regBody)
	}
	if strings.Contains(string(regBody), "auth.register.invite_note") || strings.Contains(string(regBody), `class="warning"`) {
		t.Error("с валидным токеном предупреждение «только по приглашению» лишнее")
	}

	resp := postForm(t, s.srv, "/register", url.Values{
		"email": {"newbie@corp.example"}, "password": {"correct-horse-battery"},
		"password2": {"correct-horse-battery"}, "next": {inviteAcceptPath},
	}, s.srv.URL, inviteCookie)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != inviteAcceptPath {
		t.Fatalf("после регистрации Location = %q, want %q", got, inviteAcceptPath)
	}
	if !userExists(t, s, "newbie@corp.example") {
		t.Fatal("аккаунт не создан при валидном токене")
	}
}

func TestWebInviteFormKeepsInputOn422(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "invite422-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "invite422-co", "Invite422 Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	invitePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/invite"

	resp := postForm(t, s.srv, invitePath,
		url.Values{"email": {"not-an-email"}, "role": {"admin"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST invite (bad email) status = %d, want 422", resp.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, `value="not-an-email"`) {
		t.Errorf("введённый email потерян: %s", page)
	}
	if !strings.Contains(page, `<option value="admin" selected`) {
		t.Errorf("выбранная роль потеряна: %s", page)
	}
	if !strings.Contains(page, `id="invite-error"`) {
		t.Errorf("ошибка не привязана к форме приглашения: %s", page)
	}
}

// Cookie подконтрольна клиенту (в т.ч. forged Set-Cookie с того же сайта, например через
// скомпрометированный поддомен) — защита от подстановки пути через её значение.
func TestInviteNextCookieRejectsPathBreakingValue(t *testing.T) {
	s := newInviteModeStack(t)

	for _, bad := range []string{"x/y", "a?b", "c#d"} {
		req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/login", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: "invite_next", Value: bad})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /login: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(body)

		if strings.Contains(page, "/invite/"+bad) {
			t.Errorf("плохое значение cookie %q подставилось в путь приглашения:\n%s", bad, page)
		}
		if strings.Contains(page, `name="next"`) {
			t.Errorf("плохое значение cookie %q дало форме ложного адресата:\n%s", bad, page)
		}
	}
}
