package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Отсутствие FK на projects — не упущение: ON DELETE CASCADE снёс бы заявку вместе с проектом, а
// RESTRICT запретил бы само удаление. Проверяем поведение, не факт отсутствия ключа в каталоге.
func TestMigrate0056QueueSurvivesProjectDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 55); err != nil {
		t.Fatalf("migrate to 55: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, issueID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m56', 'M56', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm56', 'M56') RETURNING id", orgID)
	mustScan(t, pool, &issueID,
		`INSERT INTO issues (project_id, fingerprint, title, culprit)
		 VALUES ($1, 'fp-m56', 'boom', 'internal/m56.go:1') RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 56); err != nil {
		t.Fatalf("migrate to 56: %v", err)
	}

	var gotTitle string
	if err := pool.QueryRow(ctx,
		"SELECT title FROM issues WHERE id = $1", issueID).Scan(&gotTitle); err != nil {
		t.Fatalf("select issues после миграции: %v (строка пропала или испорчена)", err)
	}
	if gotTitle != "boom" {
		t.Fatalf("issues.title после 0056 = %q, значение изменилось", gotTitle)
	}

	// Заявка переживает удаление проекта — то, ради чего таблица существует.
	if _, err := pool.Exec(ctx,
		"INSERT INTO project_purge_queue (project_id) VALUES ($1)", projectID); err != nil {
		t.Fatalf("вставка заявки: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID); err != nil {
		t.Fatalf("удаление проекта: %v (внешний ключ на projects запрещает удаление?)", err)
	}
	var left int64
	mustScan(t, pool, &left,
		"SELECT count(*) FROM project_purge_queue WHERE project_id = $1", projectID)
	if left != 1 {
		t.Fatalf("после удаления проекта заявок осталось %d, want 1: заявка снята каскадом и телеметрия стала неадресуемой", left)
	}

	var valid bool
	if err := pool.QueryRow(ctx,
		"SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass",
		"project_purge_queue_enqueued_at_idx").Scan(&valid); err != nil {
		t.Fatalf("project_purge_queue_enqueued_at_idx: не найден в pg_index: %v", err)
	}
	if !valid {
		t.Fatalf("project_purge_queue_enqueued_at_idx: indisvalid = false")
	}
}
