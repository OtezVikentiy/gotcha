package web_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// inviteStack — стенд в режиме invite (он же режим по умолчанию инстанса) с
// прямым доступом к сервисам: тесты этого файла проверяют не только код
// ответа, но и то, что осталось в базе — заведён ли аккаунт и появился ли
// участник. Именно эти два факта и были ценой дыры.
type inviteStack struct {
	*stack
	auth *auth.Service
	org  *org.Service
}

// seedSeq даёт уникальные slug/email внутри одного теста: seedOrgWithInvite
// может вызываться несколько раз, а slug организации и email пользователя
// уникальны на уровне схемы.
var seedSeq atomic.Int64

func newInviteModeStack(t *testing.T) *inviteStack {
	t.Helper()
	s := newStack(t)
	s.h.RegistrationMode = "invite"
	return &inviteStack{
		stack: s,
		auth:  auth.NewService(s.pool),
		org:   org.NewService(s.pool, 1_000_000),
	}
}

// seedOrgWithInvite заводит организацию с владельцем и выписывает приглашение
// на email с ролью role. Возвращает id организации и СЫРОЙ токен — тот самый,
// что уходит в ссылку из письма.
//
// Владелец — первый пользователь инстанса, поэтому bootstrap-исключение
// («первый регистрируется всегда») к последующим регистрациям уже не
// применяется и режим действует в полную силу.
func seedOrgWithInvite(t *testing.T, s *inviteStack, email string, role org.Role) (int64, string) {
	t.Helper()
	ctx := context.Background()
	n := seedSeq.Add(1)

	ownerID, err := s.auth.Register(ctx, fmt.Sprintf("seed-owner-%d@example.com", n), "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	o, err := s.org.CreateOrg(ctx, fmt.Sprintf("seed-co-%d", n), "Seed Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token, err := s.org.Invite(ctx, o.ID, email, role)
	if err != nil {
		t.Fatalf("invite %s: %v", email, err)
	}
	return o.ID, token
}

// expireInvite делает приглашение просроченным, не трогая его в остальном:
// ждать реального истечения тест не может, а подменять срок при выписке
// значило бы проверять не тот путь.
func expireInvite(t *testing.T, s *inviteStack, email string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE org_invites SET expires_at = now() - interval '1 day' WHERE email = $1", email); err != nil {
		t.Fatalf("force expire %s: %v", email, err)
	}
}

func userExists(t *testing.T, s *inviteStack, email string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM users WHERE email = $1", email).Scan(&n); err != nil {
		t.Fatalf("count users %s: %v", email, err)
	}
	return n > 0
}

func orgMemberCount(t *testing.T, s *inviteStack, orgID int64) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM org_members WHERE org_id = $1", orgID).Scan(&n); err != nil {
		t.Fatalf("count members of %d: %v", orgID, err)
	}
	return n
}

// registerForm — форма регистрации с адресатом (next). Пустой next не
// отправляется вовсе: так же ведёт себя и настоящая форма (скрытое поле
// рисуется только при непустом next).
func registerForm(email, next string) url.Values {
	f := url.Values{
		"email": {email}, "password": {"attacker-password-1"}, "password2": {"attacker-password-1"},
	}
	if next != "" {
		f.Set("next", next)
	}
	return f
}

// TestRegisterRequiresInviteToken — ЗАКРЫВАЕМАЯ ДЫРА (P0 №2 аудита
// 2026-07-30). В режиме invite знание приглашённого адреса давало аккаунт И
// членство в чужой организации с ролью приглашения. Доказательством права
// теперь служит только токен из ссылки.
func TestRegisterRequiresInviteToken(t *testing.T) {
	s := newInviteModeStack(t)
	orgID, _ := seedOrgWithInvite(t, s, "victim@corp.example", org.RoleAdmin)

	resp := postForm(t, s.srv, "/register", url.Values{
		"email": {"victim@corp.example"}, "password": {"attacker-password-1"},
		"password2": {"attacker-password-1"},
	}, s.srv.URL, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("регистрация без токена = %d, want 403", resp.StatusCode)
	}
	if userExists(t, s, "victim@corp.example") {
		t.Fatal("аккаунт создан без предъявления токена")
	}
	if orgMemberCount(t, s, orgID) != 1 {
		t.Fatal("в организации появился посторонний участник")
	}
}

// TestRegisterWithInviteTokenCreatesAccountWithoutMembership — законный путь:
// человек пришёл по ссылке, форма несёт адресата, адрес совпал с адресом
// приглашения. Аккаунт заводится, но членство ЕЩЁ НЕ выдаётся.
//
// Пришло на смену прежнему TestRegisterInviteModeAllowsInvitedEmail, который
// закреплял ровно снятое поведение: регистрация по совпадению адреса и
// немедленная выдача членства с ролью приглашения. Членство теперь появляется
// только после явного подтверждения на /invite/{token} — см.
// TestInviteAcceptGrantsMembership.
func TestRegisterWithInviteTokenCreatesAccountWithoutMembership(t *testing.T) {
	s := newInviteModeStack(t)
	orgID, token := seedOrgWithInvite(t, s, "invited@corp.example", org.RoleMember)
	next := "/invite/" + token

	resp := postForm(t, s.srv, "/register", registerForm("invited@corp.example", next), s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("регистрация по токену = %d, want 303: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Location"); got != next {
		t.Fatalf("Location = %q, want %q", got, next)
	}
	if !userExists(t, s, "invited@corp.example") {
		t.Fatal("аккаунт не создан при валидном токене и совпавшем адресе")
	}
	if n := orgMemberCount(t, s, orgID); n != 1 {
		t.Fatalf("участников = %d, want 1 (членство выдаёт только AcceptInvite)", n)
	}
	if sessionCookie(resp) == nil {
		t.Fatal("сессия не выдана после успешной регистрации")
	}
}

// TestInviteAcceptGrantsMembership — продолжение предыдущего: членство и роль
// приходят с подтверждением приглашения, а не с регистрацией.
func TestInviteAcceptGrantsMembership(t *testing.T) {
	s := newInviteModeStack(t)
	orgID, token := seedOrgWithInvite(t, s, "invited2@corp.example", org.RoleAdmin)
	next := "/invite/" + token

	resp := postForm(t, s.srv, "/register", registerForm("invited2@corp.example", next), s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("регистрация по токену = %d, want 303", resp.StatusCode)
	}
	cookie := sessionCookie(resp)
	if cookie == nil {
		t.Fatal("сессия не выдана после регистрации")
	}

	accept := postForm(t, s.srv, next, url.Values{}, s.srv.URL, cookie)
	acceptBody, _ := io.ReadAll(accept.Body)
	accept.Body.Close()
	if accept.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303: %s", next, accept.StatusCode, acceptBody)
	}

	if n := orgMemberCount(t, s, orgID); n != 2 {
		t.Fatalf("участников после принятия = %d, want 2", n)
	}
	members, err := s.org.MembersOf(context.Background(), orgID)
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	var role org.Role
	for _, m := range members {
		if m.Email == "invited2@corp.example" {
			role = m.Role
		}
	}
	if role != org.RoleAdmin {
		t.Fatalf("роль принявшего = %q, want admin (роль из приглашения)", role)
	}
}

// TestRegisterRejectsForeignInviteToken — утёкшая ссылка не даёт завести
// аккаунт на чужой адрес: токен живой, но выписан не на тот email.
//
// Совпадение адреса проверяется здесь, а не только в AcceptInvite: к моменту
// AcceptInvite аккаунт уже создан, и откатывать его нечем — на закрытом
// инстансе остался бы посторонний аккаунт.
func TestRegisterRejectsForeignInviteToken(t *testing.T) {
	s := newInviteModeStack(t)
	orgID, token := seedOrgWithInvite(t, s, "alice@corp.example", org.RoleAdmin)

	resp := postForm(t, s.srv, "/register",
		registerForm("mallory@evil.example", "/invite/"+token), s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("чужой токен = %d, want 403: %s", resp.StatusCode, body)
	}
	if userExists(t, s, "mallory@evil.example") {
		t.Fatal("аккаунт создан по приглашению, выписанному на другой адрес")
	}
	if n := orgMemberCount(t, s, orgID); n != 1 {
		t.Fatalf("участников = %d, want 1", n)
	}
}

// TestRegisterRejectsExpiredInviteToken — просроченное приглашение правом на
// регистрацию не является.
func TestRegisterRejectsExpiredInviteToken(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "late@corp.example", org.RoleMember)
	expireInvite(t, s, "late@corp.example")

	resp := postForm(t, s.srv, "/register",
		registerForm("late@corp.example", "/invite/"+token), s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("просроченный токен = %d, want 403: %s", resp.StatusCode, body)
	}
	if userExists(t, s, "late@corp.example") {
		t.Fatal("аккаунт создан по просроченному приглашению")
	}
}

// TestRegisterDenialIsIndistinguishable — все причины отказа выглядят для
// клиента одинаково. Различие между ними и было оракулом: живое приглашение
// давало 303, отсутствующее — 403, и перебором адресов выяснялось, кто
// приглашён.
//
// Тело сравнивается после замены адресата (next) на заглушку: адресат прислал
// сам клиент, эхо его собственной строки ничего о чужих приглашениях не
// сообщает. Всё остальное обязано совпадать байт в байт. Случай «адресата нет
// вовсе» сравнивается отдельно — там эхо-подстановки нет по построению.
func TestRegisterDenialIsIndistinguishable(t *testing.T) {
	s := newInviteModeStack(t)
	_, live := seedOrgWithInvite(t, s, "known@corp.example", org.RoleAdmin)
	_, dead := seedOrgWithInvite(t, s, "gone@corp.example", org.RoleMember)
	expireInvite(t, s, "gone@corp.example")

	cases := []struct {
		name  string
		email string
		next  string
	}{
		// Адресат есть, но токена в нём нет (лишний сегмент — разбор строгий).
		{"нет токена", "known@corp.example", "/invite/nothing/here"},
		{"мусорный токен", "known@corp.example", "/invite/00000000000000000000000000000000"},
		{"просроченный токен", "gone@corp.example", "/invite/" + dead},
		{"чужой адрес", "stranger@corp.example", "/invite/" + live},
	}

	var wantBody string
	for _, tc := range cases {
		resp := postForm(t, s.srv, "/register", registerForm(tc.email, tc.next), s.srv.URL, nil)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: код = %d, want 403", tc.name, resp.StatusCode)
		}
		body := strings.ReplaceAll(string(raw), url.QueryEscape(tc.next), "NEXT")
		body = strings.ReplaceAll(body, tc.next, "NEXT")
		if wantBody == "" {
			wantBody = body
			continue
		}
		if body != wantBody {
			t.Errorf("%s: тело отказа отличается от остальных:\n--- got ---\n%s\n--- want ---\n%s",
				tc.name, body, wantBody)
		}
	}

	// Отказ без адресата: тот же код и тот же текст. Тело целиком совпасть не
	// может — в нём нет ссылки с next, — но различие внесено самим клиентом.
	resp := postForm(t, s.srv, "/register", registerForm("known@corp.example", ""), s.srv.URL, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("без адресата: код = %d, want 403", resp.StatusCode)
	}
	const msg = "только по ссылке из приглашения"
	if !strings.Contains(string(raw), msg) || !strings.Contains(wantBody, msg) {
		t.Errorf("текст отказа не совпадает с остальными:\n%s", raw)
	}
}

// emailLimitPerWindow — ёмкость per-EMAIL бакета регистрации/входа. Значение
// не выведено из наблюдения, а взято из места, где ограничитель создаётся:
// internal/web/web.go, `emailLimiter: newRateLimiter(time.Now, 50, 15*time.Minute)`.
// Если порог там изменится, этот тест упадёт и потребует осознанной правки —
// это и нужно: он закрепляет наличие бакета, а не случайное число.
const emailLimitPerWindow = 50

// postRegisterFromIP шлёт форму регистрации, представляясь клиентом с адреса
// clientIP. Работает через X-Forwarded-For, которому Handler доверяет, только
// когда непосредственный пир входит в TrustedProxies (см. clientIP в
// ratelimit.go) — стенд для этого проставляет loopback доверенной сетью.
// postForm заголовки задавать не умеет, поэтому запрос собирается здесь.
func postRegisterFromIP(t *testing.T, s *inviteStack, clientIP string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+"/register", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.srv.URL)
	req.Header.Set("X-Forwarded-For", clientIP)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestRegisterEmailLimiterCapsDistributedGuessing — перебор ОДНОГО
// приглашённого адреса с пула адресов ограничен per-EMAIL бакетом.
//
// Ограничители стоят ДО гейта приглашения, поэтому отказ по мусорному токену
// расходует те же бакеты, что и вход, — но два из трёх ключуются по IP
// (loginLimiter — ip|email, ipLimiter — ip) и с каждым новым адресом атакующего
// начинаются заново. Единственное, что считает попытки на один email со всех
// адресов сразу, — emailLimiter, и проверяется здесь именно он: каждый запрос
// приходит с собственного клиентского адреса, поэтому сработать может только
// он. Уберите его из цепочки в registerSubmit — 429 не наступит вовсе и тест
// покраснеет.
//
// До исчерпания бакета каждая попытка получает обычный отказ гейта (403) —
// это подтверждает, что она дошла до гейта, а не была срезана другим
// ограничителем раньше.
//
// Сам токен — 32 случайных байта, перебрать его нельзя и без лимитов; ценность
// капа в том, что ветка отказа не стала дешёвым способом дёргать базу с любого
// числа адресов.
func TestRegisterEmailLimiterCapsDistributedGuessing(t *testing.T) {
	s := newInviteModeStack(t)
	seedOrgWithInvite(t, s, "target@corp.example", org.RoleAdmin)

	// Пир (httptest-клиент) — loopback; объявляем loopback доверенным прокси,
	// иначе X-Forwarded-For игнорируется и все запросы схлопнутся в один IP.
	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	s.h.TrustedProxies = []*net.IPNet{loopback}

	// Адреса из TEST-NET-3 (203.0.113.0/24, RFC 5737) — заведомо не loopback,
	// то есть не попадают в доверенный набор и принимаются как клиентские.
	// Их с запасом хватает на бакет: /24 против лимита в 50.
	for i := 1; i <= emailLimitPerWindow; i++ {
		resp := postRegisterFromIP(t, s, fmt.Sprintf("203.0.113.%d", i),
			registerForm("target@corp.example", fmt.Sprintf("/invite/guess-%d", i)))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("попытка %d с адреса 203.0.113.%d = %d, want 403 "+
				"(до исчерпания email-бакета отказ даёт гейт приглашения)", i, i, resp.StatusCode)
		}
	}

	// Бакет выбран: следующая попытка на тот же адрес с ЕЩЁ ОДНОГО клиентского
	// адреса упирается в per-email кап, не доходя до гейта.
	resp := postRegisterFromIP(t, s, "203.0.113.200",
		registerForm("target@corp.example", "/invite/guess-over"))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("попытка %d (новый клиентский адрес, тот же email) = %d, want 429",
			emailLimitPerWindow+1, resp.StatusCode)
	}

	// Контроль: кап именно per-EMAIL, а не общий на инстанс — другой адрес с
	// того же клиентского IP по-прежнему доходит до гейта.
	other := postRegisterFromIP(t, s, "203.0.113.200",
		registerForm("someone-else@corp.example", "/invite/guess-other"))
	io.Copy(io.Discard, other.Body)
	other.Body.Close()
	if other.StatusCode != http.StatusForbidden {
		t.Fatalf("другой email после исчерпания бакета = %d, want 403", other.StatusCode)
	}
}

// TestRegisterInviteModeRejectsUninvited — адрес без приглашения вовсе:
// самостоятельная регистрация по-прежнему закрыта.
func TestRegisterInviteModeRejectsUninvited(t *testing.T) {
	s := newInviteModeStack(t)
	seedOrgWithInvite(t, s, "somebody@example.com", org.RoleMember)

	resp := postForm(t, s.srv, "/register", registerForm("stranger@example.com", ""), s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("uninvited register status = %d, want 403: %s", resp.StatusCode, body)
	}
	if userExists(t, s, "stranger@example.com") {
		t.Fatal("аккаунт создан без приглашения")
	}
}

// TestRegisterClosedModeRejectsEvenInvited — closed отличается от invite ровно
// этим: новых аккаунтов не появляется даже по действующему приглашению — и
// теперь даже при предъявленном живом токене.
func TestRegisterClosedModeRejectsEvenInvited(t *testing.T) {
	s := newInviteModeStack(t)
	s.h.RegistrationMode = "closed"
	orgID, token := seedOrgWithInvite(t, s, "invited-closed@example.com", org.RoleMember)

	resp := postForm(t, s.srv, "/register",
		registerForm("invited-closed@example.com", "/invite/"+token), s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("closed-mode register status = %d, want 403", resp.StatusCode)
	}
	if userExists(t, s, "invited-closed@example.com") {
		t.Fatal("режим closed завёл аккаунт по токену")
	}
	if n := orgMemberCount(t, s, orgID); n != 1 {
		t.Fatalf("участников = %d, want 1", n)
	}
}

// TestRegisterFormVisibleInInviteMode — форму в режиме invite надо показывать:
// приглашённому больше некуда ввести свой адрес. В closed — заглушка.
func TestRegisterFormVisibleInInviteMode(t *testing.T) {
	s := newInviteModeStack(t)
	seedOrgWithInvite(t, s, "formcheck@example.com", org.RoleMember)

	resp, err := http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="password2"`) {
		t.Fatalf("invite mode must still render the form:\n%s", body)
	}

	s.h.RegistrationMode = "closed"
	resp, err = http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `name="password2"`) {
		t.Fatalf("closed mode must not render the form:\n%s", body)
	}
}
