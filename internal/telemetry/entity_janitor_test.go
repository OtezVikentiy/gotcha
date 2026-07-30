package telemetry_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

var entitySeq atomic.Int64

// newEntityProject создаёт организацию с проектом. Каждый тест работает в своём
// проекте: контейнер PostgreSQL переиспользуется между запусками, и общие
// project_id связали бы тесты друг с другом.
func newEntityProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := entitySeq.Add(1)
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("janitor%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Janitor',1000000) RETURNING id",
		fmt.Sprintf("janitor-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id",
		orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// insertIssue добавляет группу с заданным возрастом последнего события.
func insertIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, fingerprint string, lastSeen time.Time, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO issues (project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen)
		 VALUES ($1,$2,'boom','app.go','error',$3,$4,$4,1) RETURNING id`,
		projectID, fingerprint, status, lastSeen).Scan(&id); err != nil {
		t.Fatalf("insert issue %s: %v", fingerprint, err)
	}
	return id
}

// insertMonitorIncident добавляет монитор с инцидентом. resolvedAt == nil —
// инцидент открыт.
func insertMonitorIncident(t *testing.T, pool *pgxpool.Pool, projectID int64, startedAt time.Time, resolvedAt *time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	n := entitySeq.Add(1)
	var monitorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, kind, interval_seconds, config)
		 VALUES ($1,$2,'http',60,'{"url":"https://example.com"}'::jsonb) RETURNING id`,
		projectID, fmt.Sprintf("mon-%d", n)).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO incidents (monitor_id, started_at, resolved_at, cause)
		 VALUES ($1,$2,$3,'down') RETURNING id`,
		monitorID, startedAt, resolvedAt).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	return id
}

func issueExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM issues WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("issue exists %d: %v", id, err)
	}
	return exists
}

func incidentExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM incidents WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("incident exists %d: %v", id, err)
	}
	return exists
}

// TestEntityJanitorPurgesExpiredKeepsFresh — основное правило: группа, событий
// которой уже нет в ClickHouse, не должна оставаться в списке проблем.
func TestEntityJanitorPurgesExpiredKeepsFresh(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	old := insertIssue(t, pool, pid, "old", now.Add(-40*24*time.Hour), "unresolved")
	fresh := insertIssue(t, pool, pid, "fresh", now.Add(-2*24*time.Hour), "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: 30 * 24 * time.Hour}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if issueExists(t, pool, old) {
		t.Errorf("группа с последним событием 40 дней назад жива при сроке хранения 30 дней")
	}
	if !issueExists(t, pool, fresh) {
		t.Errorf("группа с последним событием 2 дня назад удалена при сроке хранения 30 дней")
	}
	if j.Purged() < 1 {
		t.Errorf("Purged() = %d, ожидалось хотя бы 1 удаление", j.Purged())
	}
}

// TestEntityJanitorKeepsOpenIncident — открытый инцидент описывает то, что
// происходит сейчас: его возраст не значит, что проблему можно перестать
// показывать.
func TestEntityJanitorKeepsOpenIncident(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	now := time.Now().UTC()
	longAgo := now.Add(-90 * 24 * time.Hour)
	resolved := now.Add(-89 * 24 * time.Hour)

	open := insertMonitorIncident(t, pool, pid, longAgo, nil)
	closed := insertMonitorIncident(t, pool, pid, longAgo, &resolved)

	j := &telemetry.EntityJanitor{Pool: pool, Retention: 30 * 24 * time.Hour}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !incidentExists(t, pool, open) {
		t.Errorf("открытый инцидент удалён по возрасту — активная проблема стала невидимой")
	}
	if incidentExists(t, pool, closed) {
		t.Errorf("закрытый 89 дней назад инцидент жив при сроке хранения 30 дней")
	}
}

// TestEntityJanitorPurgesBeyondOneBatch — чистка не должна останавливаться на
// первом батче: иначе при накопленном за месяцы объёме проход удалял бы
// entityBatchSize строк в час и никогда не догонял приём.
func TestEntityJanitorPurgesBeyondOneBatch(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	const total = 1500 // больше одного батча (1000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO issues (project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen)
		 SELECT $1, 'batch-' || g, 'boom', 'app.go', 'error', 'unresolved',
		        now() - interval '40 days', now() - interval '40 days', 1
		 FROM generate_series(1, $2) g`, pid, total); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	j := &telemetry.EntityJanitor{Pool: pool, Retention: 30 * 24 * time.Hour}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var left int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM issues WHERE project_id = $1", pid).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("после прохода осталось %d из %d просроченных групп — чистка остановилась на первом батче", left, total)
	}
}

// TestEntityJanitorCascadesToChildRows — удаление группы должно убирать и
// связанные с ней строки, иначе окружения и записи глушения алертов остаются
// сиротами.
func TestEntityJanitorCascadesToChildRows(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	id := insertIssue(t, pool, pid, "with-children", time.Now().UTC().Add(-40*24*time.Hour), "unresolved")
	if _, err := pool.Exec(ctx,
		"INSERT INTO issue_environments (issue_id, project_id, environment) VALUES ($1,$2,'production')", id, pid); err != nil {
		t.Fatalf("insert issue_environments: %v", err)
	}

	j := &telemetry.EntityJanitor{Pool: pool, Retention: 30 * 24 * time.Hour}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var children int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM issue_environments WHERE issue_id = $1", id).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if children != 0 {
		t.Errorf("после удаления группы осталось %d связанных окружений", children)
	}
}

// TestEntityJanitorNoRetentionKeepsEverything — без заданного срока хранения
// продукт ничего не удаляет: молча вычищать данные у того, кто хранение не
// настраивал, нельзя.
func TestEntityJanitorNoRetentionKeepsEverything(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	ancient := insertIssue(t, pool, pid, "ancient", time.Now().UTC().Add(-5*365*24*time.Hour), "unresolved")

	j := &telemetry.EntityJanitor{Pool: pool, Retention: 0}
	n, err := j.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Errorf("Tick при Retention=0 удалил %d строк", n)
	}
	if !issueExists(t, pool, ancient) {
		t.Errorf("группа пятилетней давности удалена при незаданном сроке хранения")
	}
}
