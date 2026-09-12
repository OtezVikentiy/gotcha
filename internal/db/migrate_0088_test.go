package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Существующая строка получает 'legacy' (переход без простоя); вставка БЕЗ kind после up тоже даёт
// 'legacy' — совместимость с откатом релиза на бинарь, который про kind не знает.
func TestMigrate0088ProjectKeyKind(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 87); err != nil {
		t.Fatalf("migrate to 87: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, oldKeyID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m88','M88',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m88','M88') RETURNING id", orgID)
	mustScan(t, pool, &oldKeyID,
		"INSERT INTO project_keys (project_id, public_key) VALUES ($1,'m88key') RETURNING id", projectID)

	if err := db.MigratePGTo(dsn, 88); err != nil {
		t.Fatalf("migrate to 88: %v", err)
	}

	// Существующий ключ — legacy: полный допуск, приём проекта не встал.
	var kind string
	if err := pool.QueryRow(ctx,
		"SELECT kind FROM project_keys WHERE id = $1", oldKeyID).Scan(&kind); err != nil {
		t.Fatalf("select kind: %v", err)
	}
	if kind != "legacy" {
		t.Fatalf("kind существующего ключа = %q, ожидалось legacy", kind)
	}

	if _, err := pool.Exec(ctx,
		"INSERT INTO project_keys (project_id, public_key, kind) VALUES ($1,'m88bad','root')",
		projectID); err == nil {
		t.Fatal("CHECK принял kind='root'")
	}

	for _, k := range []string{"browser", "server", "agent", "legacy"} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO project_keys (project_id, public_key, kind) VALUES ($1,$2,$3)",
			projectID, "m88ok"+k, k); err != nil {
			t.Fatalf("CHECK отверг допустимый kind=%q: %v", k, err)
		}
	}

	// Вставка без kind — то, что делает старый бинарь после отката релиза; без этого свойства откат
	// ломал бы создание ключей и следом создание проектов.
	var rolledBackKind string
	if err := pool.QueryRow(ctx,
		"INSERT INTO project_keys (project_id, public_key) VALUES ($1,'m88old') RETURNING kind",
		projectID).Scan(&rolledBackKind); err != nil {
		t.Fatalf("вставка без kind: %v", err)
	}
	if rolledBackKind != "legacy" {
		t.Fatalf("вставка без kind дала %q, ожидалось legacy", rolledBackKind)
	}

	if err := db.MigratePGTo(dsn, 87); err != nil {
		t.Fatalf("migrate down to 87: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name='project_keys' AND column_name='kind')`).Scan(&exists); err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if exists {
		t.Fatal("после down столбец kind остался")
	}
}
