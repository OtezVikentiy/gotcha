package org

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRewrapSSOSecretCAS(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool, 1_000_000)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 1000000) RETURNING id",
		"rewrapssocas").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO org_sso (org_id, issuer, client_id, client_secret, domain)
		VALUES ($1, 'https://idp.example', 'client-id', 'current-value', 'rewrapssocas.example.com')`,
		orgID); err != nil {
		t.Fatalf("org_sso: %v", err)
	}

	ok, err := svc.rewrapSSOSecret(ctx, orgID, "stale-old-value", "would-be-new")
	if err != nil {
		t.Fatalf("rewrapSSOSecret(stale): %v", err)
	}
	if ok {
		t.Fatalf("rewrapSSOSecret(stale old) = true, want false (0 rows affected)")
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id=$1", orgID).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "current-value" {
		t.Fatalf("client_secret затёрт при несовпавшем old: %q, want unchanged current-value", stored)
	}

	ok, err = svc.rewrapSSOSecret(ctx, orgID, "current-value", "new-value")
	if err != nil {
		t.Fatalf("rewrapSSOSecret(current): %v", err)
	}
	if !ok {
		t.Fatalf("rewrapSSOSecret(current old) = false, want true")
	}
	if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id=$1", orgID).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "new-value" {
		t.Fatalf("client_secret = %q, want new-value", stored)
	}
}

func TestRewrapSSOSecretExecError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool, 1_000_000)
	ctx := context.Background()
	pool.Close()

	ok, err := svc.rewrapSSOSecret(ctx, 1, "old-value", "new-value")
	if err == nil {
		t.Fatalf("rewrapSSOSecret на закрытом пуле = (%v,nil), want ненулевую ошибку", ok)
	}
	if ok {
		t.Fatalf("rewrapSSOSecret на закрытом пуле = (true,%v), want false при ошибке", err)
	}
}
