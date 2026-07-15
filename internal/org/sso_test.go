package org_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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
	// SSOByOrg — домен нормализован в lower, default_role=member.
	got, ok, err := svc.SSOByOrg(ctx, oa.ID)
	if err != nil || !ok || got.Domain != "corp.com" || got.DefaultRole != "member" || !got.Enforced {
		t.Fatalf("by org = (%+v,%v,%v)", got, ok, err)
	}
	// SSOByDomain (регистронезависимо).
	if d, ok, _ := svc.SSOByDomain(ctx, "CORP.com"); !ok || d.OrgID != oa.ID {
		t.Fatalf("by domain = (%+v,%v)", d, ok)
	}
	// Невалидный конфиг.
	if err := svc.UpsertSSO(ctx, org.SSOConfig{OrgID: oa.ID, Domain: "x.com"}); !errors.Is(err, org.ErrInvalidSSO) {
		t.Fatalf("invalid = %v, want ErrInvalidSSO", err)
	}
	// Домен занят другой организацией → ErrDomainTaken.
	if err := svc.UpsertSSO(ctx, org.SSOConfig{OrgID: ob.ID, Issuer: "https://i", ClientID: "c", ClientSecret: "s", Domain: "corp.com"}); !errors.Is(err, org.ErrDomainTaken) {
		t.Fatalf("domain taken = %v, want ErrDomainTaken", err)
	}
	// Повторный upsert своей орги обновляет (enforced → false).
	cfg.Enforced = false
	if err := svc.UpsertSSO(ctx, cfg); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got, _, _ := svc.SSOByOrg(ctx, oa.ID); got.Enforced {
		t.Fatalf("enforced should be false after update")
	}
	// Delete.
	if err := svc.DeleteSSO(ctx, oa.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := svc.SSOByOrg(ctx, oa.ID); ok {
		t.Fatalf("sso should be gone after delete")
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
	// Повторно — не ошибка, роль не меняется.
	if err := svc.EnsureMember(ctx, o.ID, u, org.RoleAdmin); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if r, err := svc.Role(ctx, o.ID, u); err != nil || r != org.RoleMember {
		t.Fatalf("role = %v err=%v, want member (unchanged)", r, err)
	}
}
