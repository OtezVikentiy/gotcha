package web

import (
	"context"
	"net/http"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestOAuthInviteProvisioningStillWorks — регрессия к гейту регистрации по
// токену (P0 №2): парольная регистрация больше не выдаёт членство по
// совпадению адреса, а OAuth-путь — по-прежнему выдаёт, и это законно.
//
// Разница в том, кто ручается за адрес. В форме за него не ручается никто:
// адрес вводит тот, кто пришёл. В OAuth-потоке адрес подтверждён провайдером
// (id.EmailVerified), и приглашение на этот адрес — доказательство права.
// Единственная законная выдача членства без токена, и она такой остаётся.
//
// Живёт в пакете web (а не web_test) ради callbackStack: подделать
// подписанную flow-cookie снаружи пакета нечем.
func TestOAuthInviteProvisioningStillWorks(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.h.RegistrationMode = "invite"

	ownerID, err := s.auth.Register(ctx, "oauth-owner@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	o, err := s.org.CreateOrg(ctx, "oauth-inv-co", "OAuth Inv Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := s.org.Invite(ctx, o.ID, "oauth-newbie@corp.com", org.RoleAdmin); err != nil {
		t.Fatalf("invite: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "sub-invite-regression", Email: "oauth-newbie@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("provisioning по приглашению = %d, want 303", resp.StatusCode)
	}

	uid, err := s.auth.UserByEmail(ctx, "oauth-newbie@corp.com")
	if err != nil {
		t.Fatalf("аккаунт не заведён: %v", err)
	}
	members, err := s.org.MembersOf(ctx, o.ID)
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	var role org.Role
	for _, m := range members {
		if m.UserID == uid {
			role = m.Role
		}
	}
	if role != org.RoleAdmin {
		t.Fatalf("роль провижининга = %q, want admin (роль из приглашения)", role)
	}
}
