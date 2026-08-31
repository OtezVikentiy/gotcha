package org

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRewrapSSOSecretCAS — whitebox, зеркало
// internal/alert.TestRewrapChannelSecretCAS: rewrapSSOSecret переписывает
// client_secret ТОЛЬКО если он всё ещё равен значению, прочитанному
// RewrapSecrets в начале партии. Устаревший old (конкурентный UpsertSSO или
// бэкфилл другой реплики между чтением партии и этим UPDATE) — ноль
// затронутых строк, а не затирание чужой записи.
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

	// Устаревший old не совпадает с фактическим значением в БД — CAS не
	// затрагивает строку.
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

	// Актуальный old — обновление проходит.
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

// TestRewrapSSOSecretExecError — обрыв соединения на самом UPDATE, зеркало
// internal/alert.TestRewrapChannelSecretExecError: rewrapSSOSecret обязан
// вернуть ошибку вызывающему, а не (false,nil) — иначе RewrapSecrets молча
// спишет реальный сбой записи на «кто-то опередил» (CAS miss) и не
// залогирует его через slog.Warn.
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
