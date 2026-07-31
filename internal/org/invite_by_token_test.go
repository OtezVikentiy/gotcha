package org_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestInviteByTokenReadsWithoutConsuming — приглашение читается по токену и
// НЕ гасится: страница должна показать, куда зовут, до того как человек
// согласился.
func TestInviteByTokenReadsWithoutConsuming(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ownerID := newUser(t, pool, "ibt-owner@example.com")
	o, err := svc.CreateOrg(ctx, "ibt", "Invite By Token", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token, err := svc.Invite(ctx, o.ID, "guest@example.com", org.RoleAdmin)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	inv, err := svc.InviteByToken(ctx, token)
	if err != nil {
		t.Fatalf("invite by token: %v", err)
	}
	if inv.OrgID != o.ID || inv.OrgName != "Invite By Token" ||
		inv.Email != "guest@example.com" || inv.Role != org.RoleAdmin {
		t.Fatalf("InviteInfo = %+v", inv)
	}

	// Не погашено: второе чтение даёт то же самое.
	if _, err := svc.InviteByToken(ctx, token); err != nil {
		t.Fatalf("повторное чтение: %v", err)
	}
}

// TestInviteByTokenRejectsDeadTokens — несуществующий, просроченный и уже
// принятый неотличимы: одна и та же ошибка. Различие было бы оракулом.
func TestInviteByTokenRejectsDeadTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ownerID := newUser(t, pool, "ibt2-owner@example.com")
	o, err := svc.CreateOrg(ctx, "ibt2", "IBT2", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	expired, err := svc.Invite(ctx, o.ID, "expired@example.com", org.RoleMember)
	if err != nil {
		t.Fatalf("invite expired: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE org_invites SET expires_at = now() - interval '1 day' WHERE email = 'expired@example.com'"); err != nil {
		t.Fatalf("force expire: %v", err)
	}
	accepted, err := svc.Invite(ctx, o.ID, "accepted@example.com", org.RoleMember)
	if err != nil {
		t.Fatalf("invite accepted: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE org_invites SET accepted_at = now() WHERE email = 'accepted@example.com'"); err != nil {
		t.Fatalf("force accept: %v", err)
	}

	for name, token := range map[string]string{
		"несуществующий": "no-such-token",
		"просроченный":   expired,
		"принятый":       accepted,
	} {
		if _, err := svc.InviteByToken(ctx, token); !errors.Is(err, org.ErrInviteInvalid) {
			t.Errorf("%s токен: err = %v, want ErrInviteInvalid", name, err)
		}
	}
}
