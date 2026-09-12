package org_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func mustKeyring(t *testing.T, raw string) secretbox.Keyring {
	t.Helper()
	ring, err := secretbox.NewKeyring(raw, "")
	if err != nil {
		t.Fatalf("NewKeyring(%q): %v", raw, err)
	}
	return ring
}

func TestSSOConfigCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	ownerA := newUser(t, pool, "sso-a@example.com")
	oa, _ := svc.CreateOrg(ctx, "sso-a", "SSO A", ownerA)
	ownerB := newUser(t, pool, "sso-b@example.com")
	ob, _ := svc.CreateOrg(ctx, "sso-b", "SSO B", ownerB)

	cfg := org.SSOConfig{OrgID: oa.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: "s", Domain: "Corp.com", Enforced: true}
	if err := svc.UpsertSSO(ctx, cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok, err := svc.SSOByOrg(ctx, oa.ID)
	if err != nil || !ok || got.Domain != "corp.com" || got.DefaultRole != "member" || !got.Enforced {
		t.Fatalf("by org = (%+v,%v,%v)", got, ok, err)
	}
	if d, ok, _ := svc.SSOByDomain(ctx, "CORP.com"); !ok || d.OrgID != oa.ID {
		t.Fatalf("by domain = (%+v,%v)", d, ok)
	}
	if err := svc.UpsertSSO(ctx, org.SSOConfig{OrgID: oa.ID, Domain: "x.com"}); !errors.Is(err, org.ErrInvalidSSO) {
		t.Fatalf("invalid = %v, want ErrInvalidSSO", err)
	}
	if err := svc.UpsertSSO(ctx, org.SSOConfig{OrgID: ob.ID, Issuer: "https://i", ClientID: "c", ClientSecret: "s", Domain: "corp.com"}); !errors.Is(err, org.ErrDomainTaken) {
		t.Fatalf("domain taken = %v, want ErrDomainTaken", err)
	}
	cfg.Enforced = false
	if err := svc.UpsertSSO(ctx, cfg); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got, _, _ := svc.SSOByOrg(ctx, oa.ID); got.Enforced {
		t.Fatalf("enforced should be false after update")
	}
	if err := svc.DeleteSSO(ctx, oa.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := svc.SSOByOrg(ctx, oa.ID); ok {
		t.Fatalf("sso should be gone after delete")
	}
}

func TestSSOSecretEncryptedAtRest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	svc.SetKeyring(mustKeyring(t, "master-key-for-sso"))
	ctx := context.Background()

	owner := newUser(t, pool, "sso-enc@example.com")
	o, _ := svc.CreateOrg(ctx, "sso-enc", "SSO Enc", owner)

	const plaintext = "super-secret-value"
	cfg := org.SSOConfig{OrgID: o.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: plaintext, Domain: "enc.com"}
	if err := svc.UpsertSSO(ctx, cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok, err := svc.SSOByOrg(ctx, o.ID)
	if err != nil || !ok {
		t.Fatalf("by org = (%v,%v)", ok, err)
	}
	if got.ClientSecret != plaintext {
		t.Fatalf("client_secret = %q, want %q", got.ClientSecret, plaintext)
	}
	if d, ok, _ := svc.SSOByDomain(ctx, "enc.com"); !ok || d.ClientSecret != plaintext {
		t.Fatalf("by domain client_secret = %q ok=%v", d.ClientSecret, ok)
	}

	var stored string
	if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id = $1", o.ID).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("stored secret %q has no enc: prefix", stored)
	}
	if strings.Contains(stored, plaintext) {
		t.Fatalf("stored secret %q leaks plaintext", stored)
	}
}

func TestSSOSecretEncryptedWithoutMasterKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	keyed := org.NewService(pool, 1_000_000)
	keyed.SetKeyring(mustKeyring(t, "master-key-for-sso-rollback"))
	owner := newUser(t, pool, "sso-rollback@example.com")
	o, _ := keyed.CreateOrg(ctx, "sso-rollback", "SSO Rollback", owner)

	const plaintext = "real-client-secret"
	cfg := org.SSOConfig{OrgID: o.ID, Issuer: "https://idp", ClientID: "c", ClientSecret: plaintext, Domain: "rollback.com"}
	if err := keyed.UpsertSSO(ctx, cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id = $1", o.ID).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("precondition: client_secret must be encrypted at rest, got %q", stored)
	}

	noKey := org.NewService(pool, 1_000_000)
	if _, ok, err := noKey.SSOByOrg(ctx, o.ID); err == nil {
		t.Fatal("SSOByOrg вернул nil-ошибку — должен отказать, а не отдать ciphertext как client_secret")
	} else if ok {
		t.Fatal("SSOByOrg вернул ok=true вместе с ошибкой")
	}
	if _, ok, err := noKey.SSOByDomain(ctx, "rollback.com"); err == nil {
		t.Fatal("SSOByDomain вернул nil-ошибку — должен отказать, а не отдать ciphertext как client_secret")
	} else if ok {
		t.Fatal("SSOByDomain вернул ok=true вместе с ошибкой")
	}
}

func TestEnsureMemberIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "em-owner@example.com")
	o, _ := svc.CreateOrg(ctx, "em", "EM", ownerID)
	u := newUser(t, pool, "em-member@example.com")

	if err := svc.EnsureMember(ctx, o.ID, u, org.RoleMember); err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	if err := svc.EnsureMember(ctx, o.ID, u, org.RoleAdmin); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if r, err := svc.Role(ctx, o.ID, u); err != nil || r != org.RoleMember {
		t.Fatalf("role = %v err=%v, want member (unchanged)", r, err)
	}
}
