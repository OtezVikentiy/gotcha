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

// TestPendingInvitesVisibleAndRevocable: выписанное приглашение обязано быть
// видно и отзываемо.
//
// Раньше оно было невидимо и неотменяемо: ошибся в адресе — ссылка ушла
// постороннему, а из интерфейса сделать было нечего, хотя в сервисе способ
// существовал.
func TestPendingInvitesVisibleAndRevocable(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, cookie := orgSettingsRegister(t, authSvc, "revoke-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "revoke-co", "Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	const wrongAddress = "typo@example.com"
	if _, err := orgSvc.Invite(context.Background(), o.ID, wrongAddress, org.RoleMember); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	settings := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"
	resp := getWithCookie(t, s.srv, settings, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), wrongAddress) {
		t.Fatalf("страница настроек не показывает выписанное приглашение: адрес %q не найден", wrongAddress)
	}

	invites, err := orgSvc.PendingInvites(context.Background(), o.ID)
	if err != nil || len(invites) != 1 {
		t.Fatalf("PendingInvites = %+v, err = %v; want ровно одно", invites, err)
	}

	// Первый POST без подтверждения — страница вопроса, приглашение цело.
	revokePath := settings + "/invite/revoke"
	form := url.Values{"invite_id": {strconv.FormatInt(invites[0].ID, 10)}, "email": {wrongAddress}}
	resp = postForm(t, s.srv, revokePath, form, s.srv.URL, cookie)
	confirmBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("подтверждение отзыва: статус %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(confirmBody), wrongAddress) {
		t.Errorf("вопрос не называет адрес — защищать от опечатки он не может:\n%s", confirmBody)
	}
	if still, _ := orgSvc.PendingInvites(context.Background(), o.ID); len(still) != 1 {
		t.Fatalf("приглашение отозвано без подтверждения")
	}

	// Второй POST с подтверждением — отзыв.
	form.Set("confirmed", "yes")
	resp = postForm(t, s.srv, revokePath, form, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("отзыв приглашения: статус %d, want 303", resp.StatusCode)
	}
	// Ключ flash-сообщения раньше жил как "flash.invite.revoked" — с точкой,
	// в белом списке flashKeys не значился, и setFlash молча ничего не
	// клал в cookie. Администратор не мог отличить «отозвано» от
	// «форма не сработала». Теперь ключ "flash.invite_revoked" в списке
	// есть, и cookie обязана появиться.
	var flashCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "flash" {
			flashCookie = c
		}
	}
	if flashCookie == nil {
		t.Fatal("отзыв приглашения не поставил flash-cookie — сообщение об успехе потеряно")
	}
	if v, err := url.QueryUnescape(flashCookie.Value); err != nil || !strings.Contains(v, "flash.invite_revoked") {
		t.Errorf("flash-cookie не несёт ключ flash.invite_revoked: %q (err=%v)", flashCookie.Value, err)
	}
	left, err := orgSvc.PendingInvites(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("PendingInvites: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("после отзыва осталось %d приглашений", len(left))
	}
}

// TestRevokeInviteIsScopedToOrg: идентификатор приходит из формы, поэтому
// администратор одной организации не должен отзывать приглашения чужой.
func TestRevokeInviteIsScopedToOrg(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	victimID, _ := orgSettingsRegister(t, authSvc, "victim-owner@example.com")
	victim, err := orgSvc.CreateOrg(context.Background(), "victim-co", "Victim", victimID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	if _, err := orgSvc.Invite(context.Background(), victim.ID, "guest@example.com", org.RoleMember); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	victimInvites, _ := orgSvc.PendingInvites(context.Background(), victim.ID)
	if len(victimInvites) != 1 {
		t.Fatalf("подготовка: приглашений %d, want 1", len(victimInvites))
	}

	attackerID, attackerCookie := orgSettingsRegister(t, authSvc, "attacker@example.com")
	attacker, err := orgSvc.CreateOrg(context.Background(), "attacker-co", "Attacker", attackerID)
	if err != nil {
		t.Fatalf("create attacker org: %v", err)
	}

	resp := postForm(t, s.srv,
		"/orgs/"+strconv.FormatInt(attacker.ID, 10)+"/settings/invite/revoke",
		url.Values{
			"invite_id": {strconv.FormatInt(victimInvites[0].ID, 10)},
			"email":     {"guest@example.com"},
			"confirmed": {"yes"},
		}, s.srv.URL, attackerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	left, err := orgSvc.PendingInvites(context.Background(), victim.ID)
	if err != nil {
		t.Fatalf("PendingInvites: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("администратор чужой организации отозвал приглашение: осталось %d, want 1", len(left))
	}
}
