package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки, а не на чистую. На момент этой правки
// новейшая — 0055_issues_culprit_trgm_idx.up.sql (см. migrate_0052_test.go
// про то, почему тот файл не переименован и не тронут: правило смотрит
// только на ТЕКУЩУЮ последнюю версию, а не на историю; этот файл — новый, по
// тому же образцу, не замена).

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0055AddsTrgmSearchIndexesWithoutTouchingData: находка №46,
// вторая половина — поиск списка проблем (issue.buildIssueFilter) идёт через
// `title ILIKE $N OR culprit ILIKE $N`, и до 0053-0055 под это не было ни
// одного индекса вообще (обычный btree по (project_id, last_seen) не
// помогает поиску подстроки в произвольном месте строки). 0053 ставит
// расширение pg_trgm, 0054/0055 — по одному GIN-индексу (gin_trgm_ops) на
// title и culprit, каждый отдельным CONCURRENTLY-файлом (см. их докблоки).
//
// Проверка содержательная, не формальная: накатываем схему только до 0052
// (до всех трёх правок), заводим НЕПУСТУЮ таблицу issues (реальная строка
// проекта с title/culprit, содержащими подстроку, по которой умеет искать
// продукт), применяем 0053-0055 и убеждаемся одновременно в трёх вещах:
// существующая строка issues не пострадала, оба индекса появились в
// каталоге, и оба ДЕЙСТВИТЕЛЬНЫ (indisvalid) — не только «CREATE INDEX не
// вернул ошибку», но и что построение реально завершилось (см. докблок 0031
// про ловушку недостроенного CONCURRENTLY-индекса: IF NOT EXISTS при
// повторном накате её не лечит, а формальная проверка «объект существует» её
// не поймала бы).
func TestMigrate0055AddsTrgmSearchIndexesWithoutTouchingData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 52); err != nil {
		t.Fatalf("migrate to 52: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, issueID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m55', 'M55', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm55', 'M55') RETURNING id", orgID)
	mustScan(t, pool, &issueID,
		`INSERT INTO issues (project_id, fingerprint, title, culprit)
		 VALUES ($1, 'fp-m55', 'panic in handler needle55', 'internal/worker/needle55.go:12')
		 RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 55); err != nil {
		t.Fatalf("migrate to 55: %v", err)
	}

	// Данные не пострадали: строка issues на месте, с теми же title/culprit.
	var gotTitle, gotCulprit string
	if err := pool.QueryRow(ctx,
		"SELECT title, culprit FROM issues WHERE id = $1", issueID,
	).Scan(&gotTitle, &gotCulprit); err != nil {
		t.Fatalf("select issues после миграции: %v (строка пропала или испорчена)", err)
	}
	if gotTitle != "panic in handler needle55" || gotCulprit != "internal/worker/needle55.go:12" {
		t.Fatalf("issues после 0053-0055 = (title=%q culprit=%q), значения изменились", gotTitle, gotCulprit)
	}

	// Расширение реально установлено (0053), а не просто "миграция не упала".
	var extCount int64
	mustScan(t, pool, &extCount, "SELECT count(*) FROM pg_extension WHERE extname = 'pg_trgm'")
	if extCount != 1 {
		t.Fatalf("pg_extension: pg_trgm найдено %d раз, want 1 — расширение не установлено", extCount)
	}

	// Оба индекса существуют И действительны — не только "объект с таким
	// именем есть в каталоге" (это могло бы означать недостроенный индекс от
	// сорванного CONCURRENTLY, см. докблок 0031), а indisvalid = true у
	// каждого: планировщик реально может ими пользоваться.
	for _, idx := range []string{"issues_title_trgm_idx", "issues_culprit_trgm_idx"} {
		var valid bool
		if err := pool.QueryRow(ctx,
			"SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass", idx,
		).Scan(&valid); err != nil {
			t.Fatalf("%s: не найден в pg_index (индекс не создан?): %v", idx, err)
		}
		if !valid {
			t.Fatalf("%s: indisvalid = false — построение CONCURRENTLY сорвалось, индекс недействителен", idx)
		}
	}
}
