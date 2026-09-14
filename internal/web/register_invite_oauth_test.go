package web

import (
	"context"
	"net/http"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// OAuth путь выдаёт членство по e-mail там, где пароль не должен: адрес подтверждён провайдером.
// тест в пакете web, не web_test: подписанную flow-cookie снаружи не подделать.
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
	s.mp.id = oauth.Identity{Subject: "sub-invite-regression", Email: "oauth-newbie@corp.com", EmailVerified: true, TrustedIssuer: true}

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
