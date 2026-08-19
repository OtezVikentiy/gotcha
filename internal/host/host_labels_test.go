package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestHostLabelsMigrationApplies проверяет, что миграция 0073 добавила
// колонки environment/role на hosts, и для уже существующих строк метка
// «неизвестна» моделируется пустой строкой (NOT NULL DEFAULT '').
func TestHostLabelsMigrationApplies(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var org, proj int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations (slug,name,event_quota) VALUES ('m0','m0',1000000) RETURNING id").Scan(&org); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO projects (org_id,slug,name) VALUES ($1,'m0','m0') RETURNING id", org).Scan(&proj); err != nil {
		t.Fatalf("proj: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO hosts (project_id, name) VALUES ($1,'h1')", proj); err != nil {
		t.Fatalf("host insert: %v", err)
	}

	var env, role string
	if err := pool.QueryRow(ctx, "SELECT environment, role FROM hosts WHERE project_id=$1 AND name='h1'", proj).Scan(&env, &role); err != nil {
		t.Fatalf("select: %v", err)
	}
	if env != "" || role != "" {
		t.Fatalf("labels = (%q,%q), want empty", env, role)
	}
}
