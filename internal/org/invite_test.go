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
