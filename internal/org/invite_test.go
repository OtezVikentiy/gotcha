package org_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestInviteFlow(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "inv-owner@example.com")
	o, err := svc.CreateOrg(ctx, "inv", "Inv", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if _, err := svc.Invite(ctx, o.ID, "new@example.com", org.RoleOwner); !errors.Is(err, org.ErrInvalidRole) {
		t.Fatalf("owner invite: got %v, want ErrInvalidRole", err)
	}

	token, err := svc.Invite(ctx, o.ID, "new@example.com", org.RoleMember)
	if err != nil || token == "" {
		t.Fatalf("Invite: %v", err)
	}

	invited := newUser(t, pool, "new@example.com")
	gotOrg, err := svc.AcceptInvite(ctx, token, invited)
	if err != nil || gotOrg != o.ID {
		t.Fatalf("AcceptInvite: org=%d err=%v", gotOrg, err)
	}
	if r, err := svc.Role(ctx, o.ID, invited); err != nil || r != org.RoleMember {
		t.Fatalf("invited role: r=%q err=%v", r, err)
	}

	// Одноразовость.
	other := newUser(t, pool, "other@example.com")
	if _, err := svc.AcceptInvite(ctx, token, other); !errors.Is(err, org.ErrInviteInvalid) {
		t.Fatalf("reused token: got %v, want ErrInviteInvalid", err)
	}
	// Мусорный токен.
	if _, err := svc.AcceptInvite(ctx, "garbage", other); !errors.Is(err, org.ErrInviteInvalid) {
		t.Fatalf("garbage token: got %v, want ErrInviteInvalid", err)
	}
}

func TestInviteExpiry(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "exp-owner@example.com")
	o, err := svc.CreateOrg(ctx, "exp", "Exp", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	token, err := svc.Invite(ctx, o.ID, "late@example.com", org.RoleMember)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_invites SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	late := newUser(t, pool, "late@example.com")
	if _, err := svc.AcceptInvite(ctx, token, late); !errors.Is(err, org.ErrInviteInvalid) {
		t.Fatalf("expired invite: got %v, want ErrInviteInvalid", err)
	}
}

func TestAcceptPendingInviteByEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	ownerID := newUser(t, pool, "owner-inv@example.com")
	o, err := svc.CreateOrg(ctx, "inv-co", "Inv Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	inviteeID := newUser(t, pool, "invitee@example.com")
	if orgID, ok, err := svc.AcceptPendingInviteByEmail(ctx, "invitee@example.com", inviteeID); err != nil || ok || orgID != 0 {
		t.Fatalf("no invite = (%d,%v,%v), want (0,false,nil)", orgID, ok, err)
	}
	if _, err := svc.Invite(ctx, o.ID, "invitee@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite: %v", err)
	}
	orgID, ok, err := svc.AcceptPendingInviteByEmail(ctx, "invitee@example.com", inviteeID)
	if err != nil || !ok || orgID != o.ID {
		t.Fatalf("accept = (%d,%v,%v), want (%d,true,nil)", orgID, ok, err, o.ID)
	}
	if _, ok, _ := svc.AcceptPendingInviteByEmail(ctx, "invitee@example.com", inviteeID); ok {
		t.Fatal("second accept must be ok=false")
	}
}

func TestHasPendingInvite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "hp-owner@example.com")
	o, _ := svc.CreateOrg(ctx, "hp-co", "HP Co", ownerID)
	if ok, _ := svc.HasPendingInvite(ctx, "nobody@example.com"); ok {
		t.Fatal("no invite → false")
	}
	if _, err := svc.Invite(ctx, o.ID, "wanted@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if ok, err := svc.HasPendingInvite(ctx, "wanted@example.com"); err != nil || !ok {
		t.Fatalf("pending = (%v,%v), want (true,nil)", ok, err)
	}
}
