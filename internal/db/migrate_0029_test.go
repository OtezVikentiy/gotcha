package db_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migration0029Path — путь к файлу миграции относительно каталога пакета:
// go test запускает тесты с рабочей директорией внутри internal/db, а
// go:embed пакует те же самые файлы без преобразований — поэтому чтение
// исходника напрямую даёт ровно то содержимое, которое увидит embed.FS.
// Пакет db_test (внешний) не видит неэкспортированную pgMigrations, поэтому
// путь через os.ReadFile, а не через саму embed.FS.
const migration0029Path = "migrations/pg/0029_team_membership_invariant.up.sql"

// TestMigration0029IsMarkedBreaking — маркер обратной совместимости 0029
// закреплён отдельно от общего стража destructiveSQL
// (internal/db/compat_internal_test.go, TestBreakingMigrationsAreMarkedBreaking).
//
// Тот страж не видит эту миграцию разрушительной: он ищет только DROP COLUMN,
// DROP TABLE, RENAME COLUMN, RENAME TO, а 0029 разрушительна через DROP
// CONSTRAINT и ALTER COLUMN ... SET NOT NULL — форм, которых страж не знает.
// Расширение списка форм — работа другого подпроекта; до тех пор для этой
// конкретной миграции, ломающей совместимость, маркер закрепляется точечно
// здесь. Без этого теста смена маркера на «yes» осталась бы незамеченной, и
// гейт схемы (internal/db/compat.go) разрешил бы откат релиза через
// необратимую миграцию.
func TestMigration0029IsMarkedBreaking(t *testing.T) {
	content, err := os.ReadFile(migration0029Path)
	if err != nil {
		t.Fatalf("read %s: %v", migration0029Path, err)
	}
	first, _, _ := strings.Cut(string(content), "\n")
	first = strings.TrimSpace(first)
	if first != "-- backward-compatible: no" {
		t.Fatalf("первая строка %s = %q, want %q", migration0029Path, first, "-- backward-compatible: no")
	}
}

// TestMigrate0029CleansOrphanedTeamMembers — миграция лечит следы дефекта на
// работающих установках: строки team_members участников, которых уже
// исключили из организации. Без чистки ограничение просто не установится, но
// проверяем именно результат — что легитимное членство осталось, а висячее нет.
func TestMigrate0029CleansOrphanedTeamMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 28); err != nil {
		t.Fatalf("migrate to 28: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, teamID, goodUser, orphanUser int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name) VALUES ('m29', 'M29') RETURNING id")
	mustScan(t, pool, &teamID,
		"INSERT INTO teams (org_id, slug, name) VALUES ($1, 'core', 'Core') RETURNING id", orgID)
	mustScan(t, pool, &goodUser,
		"INSERT INTO users (email, password_hash) VALUES ('m29-good@example.com', 'x') RETURNING id")
	mustScan(t, pool, &orphanUser,
		"INSERT INTO users (email, password_hash) VALUES ('m29-orphan@example.com', 'x') RETURNING id")

	// Легитимный участник: есть и в org_members, и в team_members.
	mustExec(t, pool,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')", orgID, goodUser)
	mustExec(t, pool,
		"INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)", teamID, goodUser)
	// Висячее членство: в org_members его нет — ровно то, что оставлял RemoveMember.
	mustExec(t, pool,
		"INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)", teamID, orphanUser)

	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate to 29: %v", err)
	}

	var users []int64
	rows, err := pool.Query(ctx, "SELECT user_id FROM team_members WHERE team_id = $1", teamID)
	if err != nil {
		t.Fatalf("select team_members: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		users = append(users, id)
	}
	if len(users) != 1 || users[0] != goodUser {
		t.Fatalf("team_members = %v, want [%d]: висячее членство не вычищено", users, goodUser)
	}

	// org_id заполнен и совпадает с организацией команды.
	var gotOrg int64
	if err := pool.QueryRow(ctx,
		"SELECT org_id FROM team_members WHERE team_id = $1", teamID).Scan(&gotOrg); err != nil {
		t.Fatalf("select org_id: %v", err)
	}
	if gotOrg != orgID {
		t.Fatalf("org_id = %d, want %d", gotOrg, orgID)
	}
}

// TestMigrate0029ReplacesTeamFK — старый одиночный внешний ключ снимается,
// два составных появляются. Проверяем имена: down-миграция восстанавливает
// именно team_members_team_id_fkey, и если имя разойдётся, откат сломается.
func TestMigrate0029ReplacesTeamFK(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 28); err != nil {
		t.Fatalf("migrate to 28: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	if !constraintExists(t, pool, "team_members_team_id_fkey") {
		t.Fatal("до 0029 нет team_members_team_id_fkey — имя ограничения изменилось, поправьте миграцию")
	}
	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate to 29: %v", err)
	}
	if constraintExists(t, pool, "team_members_team_id_fkey") {
		t.Error("team_members_team_id_fkey остался после 0029")
	}
	for _, name := range []string{"teams_id_org_key", "team_members_team_org_fk", "team_members_member_fk"} {
		if !constraintExists(t, pool, name) {
			t.Errorf("ограничение %s не создано", name)
		}
	}
}

func constraintExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_constraint c
		 JOIN pg_namespace ns ON ns.oid = c.connamespace
		 WHERE ns.nspname = 'public' AND c.conname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("constraint lookup %s: %v", name, err)
	}
	return n > 0
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustScan(t *testing.T, pool *pgxpool.Pool, dst *int64, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}
