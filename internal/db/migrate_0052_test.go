package db_test

// Раунд правок 2 (задача 2, подпроект «цена запросов»): TestLatestMigrationHasDataTest
// (internal/guards) требует, чтобы НОВЕЙШАЯ миграция PostgreSQL приезжала с
// тестом на непустой базе — db.MigratePGTo на схему, уже содержащую строки,
// а не на чистую (так, как остальной набор накатывается в CI по умолчанию).
// На момент этой правки новейшая — 0052_team_members_org_id_user_id_idx.up.sql.
//
// ВАЖНО ПРО НОМЕР (см. task-2-report.md, «Раунд правок 2»): правило смотрит
// только на ТЕКУЩУЮ последнюю версию (см. докблок TestLatestMigrationHasDataTest
// в internal/guards/migrations_test.go) — как только появится миграция с
// номером больше 0052 (например, задача 8 этого же подпроекта), это правило
// начнёт требовать db.MigratePGTo(dsn, <новый номер>) и перестанет засчитывать
// этот файл. Он не сломается и не станет лишним — останется обычным
// регрессионным тестом на 0052, просто больше не будет ЕДИНСТВЕННЫМ, кто
// удовлетворяет проверку. Переписывать номер здесь при появлении 0053+ не
// нужно: нужно завести НОВЫЙ файл под новую последнюю миграцию, по этому же
// образцу (см. также internal/db/migrate_0029_test.go — первый такой тест,
// он тоже не переименован и не удалён, когда 0029 перестала быть последней).

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0052AddsTeamMembersOrgUserIndexWithoutTouchingData: находка №45 —
// team_members несёт составное ограничение team_members_member_fk (org_id,
// user_id) → org_members(org_id, user_id) без ведущего индекса. 0052
// добавляет его через CREATE INDEX CONCURRENTLY. Проверка содержательная, не
// формальная: накатываем схему только до 0051, заводим НЕПУСТУЮ таблицу
// team_members (реальные строки со всеми тремя FK, которые эта таблица несёт
// — team_id, org_id, user_id, все NOT NULL с раунда правок 0029), применяем
// 0052 и убеждаемся одновременно в двух вещах — существующие строки не
// пострадали (CREATE INDEX CONCURRENTLY не пишет и не блокирует таблицу
// надолго, но проверяем факт, а не предположение) и индекс на самом деле
// появился в каталоге, а не просто «миграция не упала».
func TestMigrate0052AddsTeamMembersOrgUserIndexWithoutTouchingData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 51); err != nil {
		t.Fatalf("migrate to 51: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, teamID, userID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name) VALUES ('m52', 'M52') RETURNING id")
	mustScan(t, pool, &teamID,
		"INSERT INTO teams (org_id, slug, name) VALUES ($1, 'core', 'Core') RETURNING id", orgID)
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m52@example.com', 'x') RETURNING id")
	// team_members_member_fk требует, чтобы (org_id, user_id) существовала в
	// org_members, — иначе строку team_members ниже не вставить вовсе.
	mustExec(t, pool,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')", orgID, userID)
	mustExec(t, pool,
		"INSERT INTO team_members (team_id, user_id, org_id) VALUES ($1, $2, $3)", teamID, userID, orgID)

	if err := db.MigratePGTo(dsn, 52); err != nil {
		t.Fatalf("migrate to 52: %v", err)
	}

	// Данные не пострадали: строка team_members на месте, с теми же тремя
	// значениями FK, которыми её завели до миграции.
	var gotTeam, gotUser, gotOrg int64
	if err := pool.QueryRow(ctx,
		"SELECT team_id, user_id, org_id FROM team_members WHERE team_id = $1 AND user_id = $2",
		teamID, userID).Scan(&gotTeam, &gotUser, &gotOrg); err != nil {
		t.Fatalf("select team_members после миграции: %v (строка пропала или испорчена)", err)
	}
	if gotTeam != teamID || gotUser != userID || gotOrg != orgID {
		t.Fatalf("team_members после 0052 = (team=%d user=%d org=%d), want (team=%d user=%d org=%d)",
			gotTeam, gotUser, gotOrg, teamID, userID, orgID)
	}

	// Индекс действительно создан (а не просто «миграция вернула nil»).
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'team_members' AND indexname = 'team_members_org_id_user_id_idx'`,
	).Scan(&n); err != nil {
		t.Fatalf("count index: %v", err)
	}
	if n != 1 {
		t.Fatalf("team_members_org_id_user_id_idx: найдено %d, want 1 — индекс не создан", n)
	}
}
