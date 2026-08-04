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
	want := url.QueryEscape("/invite/" + token)
	if !strings.Contains(page, "/register?next="+want) {
		t.Error("нет ссылки на регистрацию с сохранением токена")
	}
	if !strings.Contains(page, "/login?next="+want) {
		t.Error("нет ссылки на вход с сохранением токена")
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
