package alert

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRewrapChannelSecretCAS — whitebox: rewrapChannelSecret обязан
// переписывать secret ТОЛЬКО если он всё ещё равен значению, прочитанному
// RewrapSecrets в начале партии. Устаревший old (строку успел поменять
// конкурентный UpdateChannel или бэкфилл другой реплики между чтением партии
// и этим UPDATE) — ноль затронутых строк, а не затирание чужой записи. Тест
// бьёт по SQL напрямую (не через полный RewrapSecrets), потому что настоящую
// гонку «между чтением и записью» внутри одного вызова детерминированно не
// воспроизвести без хуков в продуктовом коде — а CAS-условие в самом
// UPDATE проверить так можно и без них.
func TestRewrapChannelSecretCAS(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()

	var orgID, pid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 1000000) RETURNING id",
		"rewrapcas").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id",
		orgID, "rewrapcas").Scan(&pid); err != nil {
		t.Fatalf("project: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		VALUES ($1, 'webhook', true, 'https://example.com', 'current-value') RETURNING id`,
		pid).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// Устаревший old не совпадает с фактическим значением в БД — CAS не
	// затрагивает строку.
	ok, err := svc.rewrapChannelSecret(ctx, id, "stale-old-value", "would-be-new")
	if err != nil {
		t.Fatalf("rewrapChannelSecret(stale): %v", err)
	}
	if ok {
		t.Fatalf("rewrapChannelSecret(stale old) = true, want false (0 rows affected)")
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "current-value" {
		t.Fatalf("secret затёрт при несовпавшем old: %q, want unchanged current-value", stored)
	}

	// Актуальный old — обновление проходит.
	ok, err = svc.rewrapChannelSecret(ctx, id, "current-value", "new-value")
	if err != nil {
		t.Fatalf("rewrapChannelSecret(current): %v", err)
	}
	if !ok {
		t.Fatalf("rewrapChannelSecret(current old) = false, want true")
	}
	if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "new-value" {
		t.Fatalf("secret = %q, want new-value", stored)
	}
}
