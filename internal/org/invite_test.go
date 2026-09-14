package org_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	gotOrg, err := svc.AcceptInvite(ctx, token, invited, "new@example.com")
	if err != nil || gotOrg != o.ID {
		t.Fatalf("AcceptInvite: org=%d err=%v", gotOrg, err)
	}
	if r, err := svc.Role(ctx, o.ID, invited); err != nil || r != org.RoleMember {
		t.Fatalf("invited role: r=%q err=%v", r, err)
	}

	other := newUser(t, pool, "other@example.com")
	if _, err := svc.AcceptInvite(ctx, token, other, "other@example.com"); !errors.Is(err, org.ErrInviteInvalid) {
		t.Fatalf("reused token: got %v, want ErrInviteInvalid", err)
	}
	if _, err := svc.AcceptInvite(ctx, "garbage", other, "other@example.com"); !errors.Is(err, org.ErrInviteInvalid) {
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
	if _, err := svc.AcceptInvite(ctx, token, late, "late@example.com"); !errors.Is(err, org.ErrInviteInvalid) {
		t.Fatalf("expired invite: got %v, want ErrInviteInvalid", err)
	}
}

func TestInviteEmailMismatch(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "mm-owner@example.com")
	o, err := svc.CreateOrg(ctx, "mm", "MM", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	token, err := svc.Invite(ctx, o.ID, "a@x.com", org.RoleMember)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	wrong := newUser(t, pool, "b@y.com")
	if _, err := svc.AcceptInvite(ctx, token, wrong, "b@y.com"); !errors.Is(err, org.ErrInviteEmailMismatch) {
		t.Fatalf("wrong email: got %v, want ErrInviteEmailMismatch", err)
	}
	if _, err := svc.Role(ctx, o.ID, wrong); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("wrong user role: got %v, want ErrNotMember", err)
	}

	right := newUser(t, pool, "a@x.com")
	gotOrg, err := svc.AcceptInvite(ctx, token, right, "A@X.com")
	if err != nil || gotOrg != o.ID {
		t.Fatalf("AcceptInvite (right): org=%d err=%v", gotOrg, err)
	}
	if r, err := svc.Role(ctx, o.ID, right); err != nil || r != org.RoleMember {
		t.Fatalf("right role: r=%q err=%v", r, err)
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

func TestAcceptPendingInviteByEmailNoDoubleAccept(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	ownerID := newUser(t, pool, "da-owner@example.com")
	o, err := svc.CreateOrg(ctx, "da-co", "DA Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := svc.Invite(ctx, o.ID, "racer@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite: %v", err)
	}
	inviteeID := newUser(t, pool, "racer@example.com")

	const racers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	oks := make([]bool, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok, err := svc.AcceptPendingInviteByEmail(ctx, "racer@example.com", inviteeID)
			oks[i], errs[i] = ok, err
		}(i)
	}
	close(start)
	wg.Wait()

	accepted := 0
	for i := 0; i < racers; i++ {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if oks[i] {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("инвайт принят %d раз, want ровно 1 (double-accept)", accepted)
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

func TestInvitePendingCap(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ownerID := newUser(t, pool, "cap-owner@example.com")
	o, err := svc.CreateOrg(ctx, "cap-co", "Cap Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	for i := 0; i < 200; i++ {
		if _, err := svc.Invite(ctx, o.ID, fmt.Sprintf("cap-invitee-%d@example.com", i), org.RoleMember); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}

	if _, err := svc.Invite(ctx, o.ID, "cap-invitee-200@example.com", org.RoleMember); !errors.Is(err, org.ErrTooManyPendingInvites) {
		t.Fatalf("201st invite: got %v, want ErrTooManyPendingInvites", err)
	}
}

func TestInviteRevokeFreesSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ownerID := newUser(t, pool, "freeslot-owner@example.com")
	o, err := svc.CreateOrg(ctx, "freeslot-co", "Freeslot Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	for i := 0; i < 200; i++ {
		if _, err := svc.Invite(ctx, o.ID, fmt.Sprintf("freeslot-invitee-%d@example.com", i), org.RoleMember); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	if _, err := svc.Invite(ctx, o.ID, "freeslot-overflow@example.com", org.RoleMember); !errors.Is(err, org.ErrTooManyPendingInvites) {
		t.Fatalf("invite at cap: got %v, want ErrTooManyPendingInvites", err)
	}

	pending, err := svc.PendingInvites(ctx, o.ID)
	if err != nil || len(pending) == 0 {
		t.Fatalf("pending invites: len=%d err=%v", len(pending), err)
	}
	if err := svc.RevokeInvite(ctx, o.ID, pending[0].ID); err != nil {
		t.Fatalf("revoke invite: %v", err)
	}

	if _, err := svc.Invite(ctx, o.ID, "freeslot-overflow@example.com", org.RoleMember); err != nil {
		t.Fatalf("invite after revoke: got %v, want nil (slot freed)", err)
	}
}
