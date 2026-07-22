package org_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestPurgeExpiredInvites — очистка накопленных ПДн приглашённых (L15):
// просроченные и принятые инвайты удаляются, живой pending-инвайт остаётся.
func TestPurgeExpiredInvites(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "invpurge-owner@example.com")
	o, err := svc.CreateOrg(ctx, "invpurge", "InvPurge", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// Три инвайта: живой, просроченный, принятый.
	if _, err := svc.Invite(ctx, o.ID, "live@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite live: %v", err)
	}
	if _, err := svc.Invite(ctx, o.ID, "expired@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite expired: %v", err)
	}
	if _, err := svc.Invite(ctx, o.ID, "accepted@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite accepted: %v", err)
	}
	// Форсируем состояния напрямую (в обход TTL/accept-флоу).
	if _, err := pool.Exec(ctx,
		"UPDATE org_invites SET expires_at = now() - interval '1 day' WHERE email = 'expired@example.com'"); err != nil {
		t.Fatalf("force expire: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE org_invites SET accepted_at = now() WHERE email = 'accepted@example.com'"); err != nil {
		t.Fatalf("force accepted: %v", err)
	}

	n, err := svc.PurgeExpiredInvites(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged = %d, want 2 (expired + accepted)", n)
	}

	// Живой инвайт остался — считаем оставшиеся строки.
	var remaining int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM org_invites WHERE org_id = $1", o.ID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining invites = %d, want 1 (live)", remaining)
	}
}
