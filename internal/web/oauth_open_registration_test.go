package web

import (
	"context"
	"net/http"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// №96: в open-режиме OAuth-вход заводит аккаунт БЕЗ приглашения — как обещает
// документация и как делает парольная open-регистрация. Членств при этом не
// появляется (симметрия: парольная open-регистрация их тоже не выдаёт).
//
// Живёт в пакете web (а не web_test) ради callbackStack: подделать
// подписанную flow-cookie снаружи пакета нечем.
func TestOAuthOpenModeProvisionsWithoutInvite(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.h.RegistrationMode = "open"
	s.mp.id = oauth.Identity{Subject: "sub-open-noinv", Email: "open-noinv@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("open provisioning = %d, want 303", resp.StatusCode)
	}
	uid, err := s.auth.UserByEmail(ctx, "open-noinv@corp.com")
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	orgs, err := s.org.OrgsOf(ctx, uid)
	if err != nil {
		t.Fatalf("OrgsOf: %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("want no memberships, got %d", len(orgs))
	}
}

// open + действующее приглашение: аккаунт создаётся и приглашение принимается
// сразу (адрес подтверждён провайдером — та же логика, что в invite-ветке).
func TestOAuthOpenModeAcceptsPendingInvite(t *testing.T) {
	s := newCallbackStack(t)
	ctx := context.Background()
	s.h.RegistrationMode = "open"

	ownerID, err := s.auth.Register(ctx, "open-owner@corp.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	o, err := s.org.CreateOrg(ctx, "open-co", "Open Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := s.org.Invite(ctx, o.ID, "open-inv@corp.com", org.RoleAdmin); err != nil {
		t.Fatalf("invite: %v", err)
	}
	s.mp.id = oauth.Identity{Subject: "sub-open-inv", Email: "open-inv@corp.com", EmailVerified: true}

	resp := s.doCallback(t, oauthFlow{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("open provisioning with invite = %d, want 303", resp.StatusCode)
	}
	uid, err := s.auth.UserByEmail(ctx, "open-inv@corp.com")
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	members, err := s.org.MembersOf(ctx, o.ID)
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	found := false
	for _, m := range members {
		if m.UserID == uid {
			found = true
		}
	}
	if !found {
		t.Fatal("pending invite was not accepted on open provisioning")
	}
}

// Регресс invite-режима (403 незнакомцу, аккаунт не создаётся) закреплён в
// TestCallbackNoInviteRefused; closed-режим — в
// TestOAuthCallback_ClosedModeBlocksInviteProvisioning.
