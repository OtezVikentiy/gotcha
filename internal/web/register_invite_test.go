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

// Тесты проверяют не только код ответа, но и состояние БД — учётку и участника: именно эти
// два факта были ценой дыры.
type inviteStack struct {
	*stack
	auth *auth.Service
	org  *org.Service
}

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

// Владелец — первый пользователь инстанса: bootstrap-исключение «первый регистрируется
// всегда» к последующим уже не применяется, режим действует в полную силу.
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

// Ждать реального истечения тест не может, а подменять срок при выписке значило бы
// проверять не тот путь.
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

// Пустой next не отправляется вовсе — так же ведёт себя настоящая форма (скрытое поле
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

// Знание приглашённого адреса раньше давало аккаунт и членство в чужой организации —
// доказательством права теперь служит только токен из ссылки.
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

// Аккаунт заводится по валидному токену, но членство выдаётся только после подтверждения
// на /invite/{token} — см. TestInviteAcceptGrantsMembership.
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

// Совпадение адреса проверяется уже здесь, не только в AcceptInvite: к моменту AcceptInvite
// аккаунт уже создан, откатывать нечем — остался бы посторонний аккаунт на закрытом инстансе.
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

// Различие в статусе/тексте между причинами отказа было оракулом: перебором адресов можно
// было выяснить, кто приглашён — тело сравнивается после замены next-адресата на заглушку.
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

// denyRegistration обязан перевзвести invite-cookie тем же токеном, что пришёл в next — иначе
// истёкшая по TTL кука не восстановится, и ссылка «войти» на экране отказа ведёт в никуда.
func TestRegisterDenialRearmsInviteCookie(t *testing.T) {
	s := newInviteModeStack(t)
	_, token := seedOrgWithInvite(t, s, "cookie-rearm@corp.example", org.RoleMember)
	expireInvite(t, s, "cookie-rearm@corp.example")
	next := "/invite/" + token

	// Кука сознательно не отправляется — сымитирован её истёкший TTL.
	resp := postForm(t, s.srv, "/register", registerForm("cookie-rearm@corp.example", next), s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("просроченный токен = %d, want 403", resp.StatusCode)
	}

	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "invite_next" {
			got = c
		}
	}
	if got == nil {
		t.Fatal("denyRegistration не перевзвёл invite-cookie — адресат теряется после истечения TTL")
	}
	if got.Value != token {
		t.Fatalf("invite-cookie перевзведена с токеном %q, want %q", got.Value, token)
	}
	if got.MaxAge <= 0 {
		t.Fatalf("invite-cookie перевзведена с MaxAge = %d, want > 0 (срок должен отсчитаться заново)", got.MaxAge)
	}
}

// Значение — из web.go (`emailLimiter: newRateLimiter(time.Now, 50, ...)`), не наблюдением:
// смена порога там роняет тест и требует осознанной правки.
const emailLimitPerWindow = 50

// X-Forwarded-For доверяется только когда пир — доверенный прокси (см. clientIP) — стенд
// объявляет loopback доверенным. postForm заголовки не умеет, поэтому запрос собран здесь.
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

// emailLimiter — единственный лимитер, ключующийся по email, а не IP: он один ловит перебор
// одного адреса с пула IP, которые обходят per-IP лимитеры (login/ip).
func TestRegisterEmailLimiterCapsDistributedGuessing(t *testing.T) {
	s := newInviteModeStack(t)
	seedOrgWithInvite(t, s, "target@corp.example", org.RoleAdmin)

	// Пир (httptest) — loopback: без доверия к нему XFF игнорируется и все запросы
	// схлопнутся в один IP.
	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	s.h.TrustedProxies = []*net.IPNet{loopback}

	// TEST-NET-3 (203.0.113.0/24, RFC 5737) — не loopback, принимается как клиентский; /24 с
	// запасом хватает на бакет (лимит 50).
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

	resp := postRegisterFromIP(t, s, "203.0.113.200",
		registerForm("target@corp.example", "/invite/guess-over"))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("попытка %d (новый клиентский адрес, тот же email) = %d, want 429",
			emailLimitPerWindow+1, resp.StatusCode)
	}

	other := postRegisterFromIP(t, s, "203.0.113.200",
		registerForm("someone-else@corp.example", "/invite/guess-other"))
	io.Copy(io.Discard, other.Body)
	other.Body.Close()
	if other.StatusCode != http.StatusForbidden {
		t.Fatalf("другой email после исчерпания бакета = %d, want 403", other.StatusCode)
	}
}

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

// Лимитер срабатывает раньше проверки токена приглашения (см. registerSubmit) — форма,
// возвращаемая на этой ветке, обязана нести ту же invite-only врезку, что и обычный GET.
func TestRegisterRateLimitedKeepsInviteNote(t *testing.T) {
	s := newInviteModeStack(t)
	seedOrgWithInvite(t, s, "somebody@example.com", org.RoleMember)

	var last *http.Response
	var lastBody string
	for i := 0; i < 6; i++ {
		resp := postForm(t, s.srv, "/register", registerForm("stranger@example.com", ""), s.srv.URL, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		last, lastBody = resp, string(body)
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6-я попытка регистрации status = %d, want 429: %s", last.StatusCode, lastBody)
	}
	if !strings.Contains(lastBody, "Регистрация на этом инстансе — только по приглашению") {
		t.Fatalf("форма после рейт-лимита потеряла invite-only врезку: %s", lastBody)
	}
}

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

func TestRegisterModeCopy(t *testing.T) {
	get := func(t *testing.T, srvURL, path string) string {
		t.Helper()
		resp, err := http.Get(srvURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}

	t.Run("invite form warns without token", func(t *testing.T) {
		s := newInviteModeStack(t)
		_, token := seedOrgWithInvite(t, s, "copycheck@example.com", org.RoleMember)

		body := get(t, s.srv.URL, "/register")
		if !strings.Contains(body, `name="password2"`) {
			t.Fatalf("invite mode must render the form:\n%s", body)
		}
		if !strings.Contains(body, `class="warning"`) {
			t.Fatalf("invite mode without token must warn about invite-only sign-up:\n%s", body)
		}

		body = get(t, s.srv.URL, "/register?next="+url.QueryEscape("/invite/"+token))
		if strings.Contains(body, `class="warning"`) {
			t.Fatalf("invite link flow must not warn:\n%s", body)
		}
	})

	t.Run("closed stub copy has no invite advice", func(t *testing.T) {
		s := newInviteModeStack(t)
		seedOrgWithInvite(t, s, "closedcopy@example.com", org.RoleMember)
		s.h.RegistrationMode = "closed"

		body := get(t, s.srv.URL, "/register")
		if !strings.Contains(body, "Регистрация закрыта") {
			t.Fatalf("closed stub must carry the closed title:\n%s", body)
		}
		if strings.Contains(body, "приглашен") {
			t.Fatalf("closed stub must not suggest invites:\n%s", body)
		}
	})

	t.Run("denials carry a single mode-true message", func(t *testing.T) {
		s := newInviteModeStack(t)
		seedOrgWithInvite(t, s, "denycopy@example.com", org.RoleMember)

		resp := postForm(t, s.srv, "/register", registerForm("denycopy@example.com", ""), s.srv.URL, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("invite denial status = %d, want 403", resp.StatusCode)
		}
		if !strings.Contains(string(body), `class="error"`) {
			t.Fatalf("invite denial must show the error banner:\n%s", body)
		}
		if strings.Contains(string(body), "администратору инстанса") {
			t.Fatalf("invite denial must not duplicate the info paragraph:\n%s", body)
		}

		s.h.RegistrationMode = "closed"
		resp = postForm(t, s.srv, "/register", registerForm("denycopy2@example.com", ""), s.srv.URL, nil)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("closed denial status = %d, want 403", resp.StatusCode)
		}
		if !strings.Contains(string(body), "новые аккаунты не заводятся") {
			t.Fatalf("closed denial must state registration is disabled:\n%s", body)
		}
		if strings.Contains(string(body), "приглашен") {
			t.Fatalf("closed denial must not suggest invites:\n%s", body)
		}
	})
}
