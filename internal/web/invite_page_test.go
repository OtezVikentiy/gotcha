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

// TestInvitePageGuidesAnonymous — аноним по ссылке видит, КУДА его зовут, и
// получает оба пути внутрь. Без этого страница показывала форму принятия,
// которая для неавторизованного молча уводила на /login, теряя токен, — из-за
// чего поток и замкнули когда-то по email.
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

	// seedOrgWithInvite заводит организацию с фиксированным названием "Seed
	// Co" (slug уникален через seedSeq, название — нет): проверяем именно его.
	if !strings.Contains(page, "Seed Co") {
		t.Error("страница не называет организацию — человек подтверждает вслепую")
	}
	// Раунд правок 1 (решение владельца): адрес приглашения НЕ показывается —
	// страница публична, а токен утекает реальными способами (переслали в
	// чат, история браузера, Referer). Гейт регистрации требует токен И
	// совпадение адреса именно затем, чтобы одного утёкшего токена было
	// недостаточно; показ адреса здесь свёл бы это требование на нет.
	if strings.Contains(page, "guest@corp.example") {
		t.Error("страница не должна называть адрес приглашения — держатель токена не обязан быть приглашённым")
	}
	// Роль выводится человеческой подписью (memberRoleLabelKey), а не сырым
	// значением "member" — иначе аноним видит служебное имя роли.
	if !strings.Contains(page, "Участник") {
		t.Error("роль должна выводиться человеческой подписью, а не сырым значением")
	}
	// K9-19: токен приглашения не должен появляться в query ни в одной ссылке
	// страницы — ни в /login, ни в /register. Раньше обе ссылки несли
	// next=/invite/{token}, и адрес утекал в историю браузера, в лог
	// обратного прокси и в Referer при переходе с этих страниц вовне.
	if strings.Contains(page, url.QueryEscape(token)) {
		t.Error("токен приглашения не должен встречаться в query — он был найден на странице закодированным")
	}
	if !strings.Contains(page, `href="/register"`) {
		t.Error("нет голой ссылки на регистрацию (без next в query)")
	}
	if !strings.Contains(page, `href="/login"`) {
		t.Error("нет голой ссылки на вход (без next в query)")
	}
	// Адресат переживает переход не через query, а через invite-cookie —
	// HttpOnly, чтобы не читалась ни JS, ни через XSS.
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

// TestInvitePageAnonymousLoginRoundTrip — полный маршрут анонима: страница
// приглашения → «войти» (без токена в query) → вход по cookie → назад на
// /invite/{token}, тоже без токена в query. Мутация, обратная K9-19: если
// token вернуть в query (см. проверки ниже), тест обязан упасть.
func TestInvitePageAnonymousLoginRoundTrip(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "roundtrip@corp.example", "member")
	inviteAcceptPath := "/invite/" + token

	if _, err := s.auth.Register(t.Context(), "roundtrip@corp.example", "correct-horse-battery"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Шаг 1: аноним видит страницу приглашения, получает invite-cookie.
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

	// Шаг 2: GET /login БЕЗ next в query (ровно так ссылка со страницы
	// приглашения теперь и устроена) — форма всё равно должна знать адресата
	// через cookie.
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

	// Шаг 3: вход — POST несёт next скрытым полем формы (не query), это не
	// предмет находки K9-19 (тело POST не попадает ни в адресную строку, ни в
	// Referer, ни в типичный лог прокси).
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
	// Invite-cookie одноразовая: успешный вход её гасит.
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" && c.Value != "" {
			t.Error("invite-cookie должна быть погашена после успешного входа")
		}
	}
}

// TestInvitePageHidesDeadToken — мёртвый токен неотличим от несуществующего.
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
	// Мёртвый токен не даёт ни ссылок входа/регистрации (см. шаблон
	// InviteAccept), ни invite-cookie — запоминать нечего.
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			t.Error("invite-cookie не должна выставляться для несуществующего/просроченного токена")
		}
	}
}

// TestInvitePageAuthenticatedShowsAcceptForm — авторизованный держатель
// токена по-прежнему видит форму принятия (а не ссылки входа/регистрации,
// которые ему не нужны) — существующее поведение не должно было измениться.
func TestInvitePageAuthenticatedShowsAcceptForm(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "member-to-be@corp.example", "member")

	// Заводим отдельного пользователя и выпускаем ему сессию напрямую (минуя
	// форму входа — это не предмет теста), чтобы получить cookie для
	// authenticated-запроса.
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
	// Авторизованному не нужна ссылка на вход, значит и invite-cookie не
	// выставляется — ей некуда пригодиться.
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			t.Error("авторизованному invite-cookie не нужна")
		}
	}
}

// TestInvitePageAnonymousRegisterRoundTrip — тот же маршрут, что
// TestInvitePageAnonymousLoginRoundTrip, но для «не зарегистрирован» ветки:
// страница приглашения → «создать аккаунт» (без токена в query) → регистрация
// по cookie → назад на /invite/{token}.
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

	// GET /register БЕЗ next в query — ровно так теперь и устроена ссылка
	// «создать аккаунт» со страницы приглашения.
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

// TestWebInviteFormKeepsInputOn422 — 422 формы приглашения сохраняет ввод
// (№27): email и выбранная роль возвращаются в форму, ошибка рендерится у
// самой формы (invite-error), а не абзацем под h1.
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
