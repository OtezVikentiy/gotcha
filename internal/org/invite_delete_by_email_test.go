package org_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestDeleteInvitesByEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	ownerID := newUser(t, pool, "delinv-owner@example.com")
	o1, err := svc.CreateOrg(ctx, "delinv-1", "DelInv1", ownerID)
	if err != nil {
		t.Fatalf("create org 1: %v", err)
	}
	o2, err := svc.CreateOrg(ctx, "delinv-2", "DelInv2", ownerID)
	if err != nil {
		t.Fatalf("create org 2: %v", err)
	}

	const nobody = "delinv-nobody@example.com"
	if n, err := svc.DeleteInvitesByEmail(ctx, nobody); err != nil || n != 0 {
		t.Fatalf("delete on empty = (%d,%v), want (0,nil)", n, err)
	}

	const bystander = "delinv-bystander@example.com"
	if _, err := svc.Invite(ctx, o1.ID, bystander, org.RoleMember); err != nil {
		t.Fatalf("invite bystander: %v", err)
	}

	const single = "delinv-single@example.com"
	if _, err := svc.Invite(ctx, o1.ID, single, org.RoleMember); err != nil {
		t.Fatalf("invite single: %v", err)
	}
	if n, err := svc.DeleteInvitesByEmail(ctx, single); err != nil || n != 1 {
		t.Fatalf("delete single = (%d,%v), want (1,nil)", n, err)
	}
	assertInviteCount(t, ctx, pool, single, 0)

	const multi = "delinv-multi@example.com"
	if _, err := svc.Invite(ctx, o1.ID, multi, org.RoleMember); err != nil {
		t.Fatalf("invite multi org1: %v", err)
	}
	if _, err := svc.Invite(ctx, o2.ID, multi, org.RoleAdmin); err != nil {
		t.Fatalf("invite multi org2: %v", err)
	}
	if n, err := svc.DeleteInvitesByEmail(ctx, multi); err != nil || n != 2 {
		t.Fatalf("delete multi = (%d,%v), want (2,nil)", n, err)
	}
	assertInviteCount(t, ctx, pool, multi, 0)

	assertInviteCount(t, ctx, pool, bystander, 1)
}

func assertInviteCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM org_invites WHERE email = $1", email).Scan(&got); err != nil {
		t.Fatalf("count invites for %s: %v", email, err)
	}
	if got != want {
		t.Fatalf("invites for %s = %d, want %d", email, got, want)
	}
}
