package org_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Тот же зафиксированный v1-вектор, что и internal/secretbox/secretbox_test.go
// и internal/alert/rewrap_secrets_test.go (literal скопирован, а не получен
// через публичный API кольца — способа запечатать v1 через Keyring в
// продуктовом коде больше нет намеренно).
const (
	rewrapV1Master   = "vector-master-v1-legacy-old-code"
	rewrapV1Plain    = "legacy-v1-secret-value"
	rewrapV1Envelope = "enc:AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYudf0xP3/sKnysGe0CDB7Uzw42DGYRgM/gl3FF8KMFQgpVnZw4I4="
)

// TestSSORewrapSecrets — бэкфилл §6 спеки ротации для org_sso.client_secret,
// зеркало TestChannelsRewrapSecrets в internal/alert: поднимает всё читаемое
// (legacy plaintext, v1, v2 предыдущим ключом) до v2 текущего, не трогает
// уже-текущее и нечитаемое, идемпотентен.
func TestSSORewrapSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ring, err := secretbox.NewKeyring("rewrap-sso-current-master", rewrapV1Master)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc := org.NewService(pool, 1_000_000)
	svc.SetKeyring(ring)
	ctx := context.Background()

	oldRing, err := secretbox.NewKeyring(rewrapV1Master, "")
	if err != nil {
		t.Fatalf("NewKeyring(old): %v", err)
	}
	v2Prev, err := oldRing.Seal("old-v2-sso-secret")
	if err != nil {
		t.Fatalf("Seal(old): %v", err)
	}

	garbageRing, err := secretbox.NewKeyring("totally-unrelated-sso-master-xyz", "")
	if err != nil {
		t.Fatalf("NewKeyring(garbage): %v", err)
	}
	garbage, err := garbageRing.Seal("garbage-sso-secret")
	if err != nil {
		t.Fatalf("Seal(garbage): %v", err)
	}

	v2Current, err := ring.Seal("already-current-sso-secret")
	if err != nil {
		t.Fatalf("Seal(current): %v", err)
	}

	orgPlain := newOrgWithSSO(t, pool, "rewrapsso-plain", "legacy-plaintext-sso-secret")
	orgV1 := newOrgWithSSO(t, pool, "rewrapsso-v1", rewrapV1Envelope)
	orgV2Prev := newOrgWithSSO(t, pool, "rewrapsso-v2prev", v2Prev)
	orgV2Cur := newOrgWithSSO(t, pool, "rewrapsso-v2cur", v2Current)
	orgUnreadable := newOrgWithSSO(t, pool, "rewrapsso-bad", garbage)
	orgEmpty := newOrgWithSSO(t, pool, "rewrapsso-empty", "")

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("RewrapSecrets: %v", err)
	}
	if updated != 3 {
		t.Fatalf("RewrapSecrets updated = %d, want 3 (plain, v1, v2-prev)", updated)
	}

	readSecret := func(orgID int64) string {
		t.Helper()
		var stored string
		if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id=$1", orgID).Scan(&stored); err != nil {
			t.Fatalf("read secret org %d: %v", orgID, err)
		}
		return stored
	}
	wantCurrentV2 := func(orgID int64, wantOpen string) {
		t.Helper()
		stored := readSecret(orgID)
		if !strings.HasPrefix(stored, "enc:v2:"+ring.CurrentID()+":") {
			t.Fatalf("org %d client_secret = %q, want v2 envelope with current key id %s", orgID, stored, ring.CurrentID())
		}
		got, err := ring.Open(stored)
		if err != nil || got != wantOpen {
			t.Fatalf("Open(org %d) = (%q,%v), want (%q,nil)", orgID, got, err, wantOpen)
		}
	}
	wantCurrentV2(orgPlain, "legacy-plaintext-sso-secret")
	wantCurrentV2(orgV1, rewrapV1Plain)
	wantCurrentV2(orgV2Prev, "old-v2-sso-secret")

	if got := readSecret(orgUnreadable); got != garbage {
		t.Fatalf("нечитаемый client_secret изменён: %q, want unchanged %q", got, garbage)
	}
	if got := readSecret(orgV2Cur); got != v2Current {
		t.Fatalf("v2-текущий client_secret изменён: %q, want unchanged %q", got, v2Current)
	}
	// Пустой client_secret НЕ запечатан: пустое значение здесь означает
	// "SSO без секрета" (или ещё не заполнено), а не значение для шифрования.
	if got := readSecret(orgEmpty); got != "" {
		t.Fatalf("пустой client_secret запечатан: %q, want \"\"", got)
	}

	updated2, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("RewrapSecrets (2nd pass): %v", err)
	}
	if updated2 != 0 {
		t.Fatalf("RewrapSecrets (2nd pass) updated = %d, want 0", updated2)
	}
}

// TestSSORewrapSecretsNoKey — без заданного кольца проход — no-op.
func TestSSORewrapSecretsNoKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	orgID := newOrgWithSSO(t, pool, "rewrapssonokey", "plain-secret")

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil || updated != 0 {
		t.Fatalf("RewrapSecrets без ключа = (%d,%v), want (0,nil)", updated, err)
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT client_secret FROM org_sso WHERE org_id=$1", orgID).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "plain-secret" {
		t.Fatalf("client_secret изменён без ключа: %q", stored)
	}
}

// newOrgWithSSO заводит организацию и её org_sso напрямую SQL с заданным
// client_secret as is — в отличие от UpsertSSO, ничего не запечатывает и не
// валидирует issuer/client_id, так тест управляет точным байтовым
// содержимым секрета.
func newOrgWithSSO(t *testing.T, pool *pgxpool.Pool, slug, secret string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO org_sso (org_id, issuer, client_id, client_secret, domain)
		VALUES ($1, 'https://idp.example', 'client-id', $2, $3)`,
		orgID, secret, slug+".example.com"); err != nil {
		t.Fatalf("org_sso: %v", err)
	}
	return orgID
}
