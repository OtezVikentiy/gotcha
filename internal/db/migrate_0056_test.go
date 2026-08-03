package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки, а не на чистую. На момент этой правки
// новейшая — 0056_project_purge_queue.up.sql (см. migrate_0055_test.go про то,
// почему предыдущие файлы не переименовываются: правило смотрит только на
// ТЕКУЩУЮ последнюю версию).

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0056QueueSurvivesProjectDeletion — содержательная проверка того
// единственного свойства, ради которого таблица заводится: заявка обязана
// пережить строку projects.
//
// Отсутствие внешнего ключа на projects — не упущение, а условие
// работоспособности (см. докблок миграции): ON DELETE CASCADE снёс бы заявку
// вместе с проектом, ради очистки которого она заведена, а RESTRICT запретил
// бы само удаление. Проверяем не «ключа нет в каталоге» (это формальность,
// которая молчала бы, добавь кто-нибудь ключ с другим именем), а поведение:
// проект удаляется, заявка остаётся, и идентификатор в ней прежний.
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

	// База непустая на момент наката: организация, проект и группа.
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

	// Данные не пострадали.
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

	// Индекс под выборку следующей заявки существует и действителен.
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
