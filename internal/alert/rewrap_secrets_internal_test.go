package alert

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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

func TestRewrapChannelSecretExecError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()
	pool.Close()

	ok, err := svc.rewrapChannelSecret(ctx, 1, "old-value", "new-value")
	if err == nil {
		t.Fatalf("rewrapChannelSecret на закрытом пуле = (%v,nil), want ненулевую ошибку", ok)
	}
	if ok {
		t.Fatalf("rewrapChannelSecret на закрытом пуле = (true,%v), want false при ошибке", err)
	}
}
