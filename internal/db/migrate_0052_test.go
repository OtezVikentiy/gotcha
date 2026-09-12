package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// team_members_member_fk (org_id, user_id) → org_members не имел ведущего индекса — 0052 добавляет
// его CONCURRENTLY; проверяем, что данные не пострадали и индекс реально появился.
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
	// team_members_member_fk требует (org_id, user_id) в org_members — иначе INSERT ниже не пройдёт.
	mustExec(t, pool,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')", orgID, userID)
	mustExec(t, pool,
		"INSERT INTO team_members (team_id, user_id, org_id) VALUES ($1, $2, $3)", teamID, userID, orgID)

	if err := db.MigratePGTo(dsn, 52); err != nil {
		t.Fatalf("migrate to 52: %v", err)
	}

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
