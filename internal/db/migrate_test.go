package db_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestMigratePG(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG: %v", err)
	}
	// Идемпотентность: повторный прогон не ошибка.
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG (second run): %v", err)
	}

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()
	var n int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_extension WHERE extname = 'citext'").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("citext extension not installed: n=%d err=%v", n, err)
	}
}

func TestMigrateCHAndRetention(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH (second run): %v", err)
	}

	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()

	showCreate := func(table string) string {
		var ddl string
		if err := conn.QueryRow(ctx, "SHOW CREATE TABLE "+table).Scan(&ddl); err != nil {
			t.Fatalf("SHOW CREATE TABLE %s: %v", table, err)
		}
		return ddl
	}

	// ClickHouse desugars "INTERVAL N DAY" into "toIntervalDay(N)" at parse
	// time; SHOW CREATE TABLE reflects the parsed AST, not the original
	// migration source text. This is server-side normalization, not a
	// property of the driver or of our SQL (which uses the INTERVAL syntax
	// verbatim, per spec §5).
	ddl := showCreate("events")
	for _, want := range []string{"event_id", "project_id", "issue_id", "toYYYYMM(timestamp)", "toIntervalDay(90)"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("events DDL missing %q:\n%s", want, ddl)
		}
	}

	crDDL := showCreate("check_results")
	for _, want := range []string{
		"monitor_id", "project_id", "region", "status_code",
		"toYYYYMM(timestamp)", "ORDER BY (monitor_id, region, timestamp)", "toIntervalDay(90)",
	} {
		if !strings.Contains(crDDL, want) {
			t.Errorf("check_results DDL missing %q:\n%s", want, crDDL)
		}
	}

	// 0003: транзакции, спаны, агрегирующая MV и trace-колонки в events.
	txDDL := showCreate("transactions")
	for _, want := range []string{
		"trace_id", "span_id", "transaction", "duration_us", "tags", "source",
		"toYYYYMM(timestamp)", "ORDER BY (project_id, transaction, timestamp)", "toIntervalDay(90)",
	} {
		if !strings.Contains(txDDL, want) {
			t.Errorf("transactions DDL missing %q:\n%s", want, txDDL)
		}
	}

	spansDDL := showCreate("spans")
	for _, want := range []string{
		"parent_span_id", "description_hash", "data",
		"toYYYYMM(timestamp)", "ORDER BY (project_id, trace_id, timestamp)", "toIntervalDay(30)",
	} {
		if !strings.Contains(spansDDL, want) {
			t.Errorf("spans DDL missing %q:\n%s", want, spansDDL)
		}
	}

	// Содержимое MV не проверяем по подстрокам — оно проверяется поведением,
	// см. TestTransactions5mAggregates.
	for _, want := range []string{"trace_id", "span_id"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("events DDL missing trace column %q:\n%s", want, ddl)
		}
	}

	if err := db.ApplyRetention(ctx, conn, 180); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if ddl := showCreate("events"); !strings.Contains(ddl, "toIntervalDay(180)") {
		t.Errorf("events TTL not updated to 180 days:\n%s", ddl)
	}
	if ddl := showCreate("check_results"); !strings.Contains(ddl, "toIntervalDay(180)") {
		t.Errorf("check_results TTL not updated to 180 days:\n%s", ddl)
	}
	// Идемпотентность: повторный вызов не должен падать, когда TTL уже совпадает.
	if err := db.ApplyRetention(ctx, conn, 180); err != nil {
		t.Fatalf("ApplyRetention (second run, same days): %v", err)
	}

	// Спаны ретенируются отдельным числом дней (GOTCHA_SPAN_RETENTION_DAYS),
	// а не вместе с events/check_results. Стартовое значение из миграции — 30.
	if err := db.ApplySpanRetention(ctx, conn, 15); err != nil {
		t.Fatalf("ApplySpanRetention: %v", err)
	}
	if ddl := showCreate("spans"); !strings.Contains(ddl, "toIntervalDay(15)") {
		t.Errorf("spans TTL not updated to 15 days:\n%s", ddl)
	}
	// Спан-ретенция не должна трогать TTL events/check_results (180 дней).
	if ddl := showCreate("events"); !strings.Contains(ddl, "toIntervalDay(180)") {
		t.Errorf("events TTL changed by span retention:\n%s", ddl)
	}
	// Идемпотентность: повторный вызов с тем же числом дней — no-op, не падает.
	if err := db.ApplySpanRetention(ctx, conn, 15); err != nil {
		t.Fatalf("ApplySpanRetention (second run, same days): %v", err)
	}
}

func TestWithMigrationLockSerializes(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()

	var mu sync.Mutex
	var inside, maxInside int
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithMigrationLock(ctx, pool, func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithMigrationLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("critical section overlapped: max concurrent = %d", maxInside)
	}
}

func TestMigratePGUpDownUp(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDownPG(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func TestMigrateCHUpDownUp(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDownCH(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}

	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()

	// Down полностью зеркалит up: ни таблиц, ни MV не остаётся.
	for _, table := range []string{"events", "check_results", "transactions", "spans", "transactions_5m"} {
		var n uint64
		err := conn.QueryRow(ctx,
			"SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = ?",
			table).Scan(&n)
		if err != nil {
			t.Fatalf("system.tables %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s still exists after down", table)
		}
	}

	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// TestTransactions5mAggregates закрепляет MV transactions_5m поведением, а не
// подстроками в DDL: вставляем строки в transactions и читаем агрегаты через
// -Merge. Заодно доказывает, что MV вообще наполняется вставками.
func TestTransactions5mAggregates(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const projectID uint64 = 42
	ts := time.Date(2026, 7, 14, 12, 3, 30, 0, time.UTC) // bucket 12:00
	rows := []struct {
		durationUS uint32
		status     string
	}{
		{1000, "ok"},
		{2000, "internal_error"},
		{3000, "ok"},
	}

	batch, err := conn.PrepareBatch(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, op, timestamp, duration_us, status, environment)")
	if err != nil {
		t.Fatalf("PrepareBatch transactions: %v", err)
	}
	for i, r := range rows {
		err := batch.Append(projectID, "trace", "span", "GET /checkout", "http.server",
			ts.Add(time.Duration(i)*time.Second), r.durationUS, r.status, "production")
		if err != nil {
			t.Fatalf("append row %d: %v", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send transactions batch: %v", err)
	}

	var (
		cnt, failures uint64
		totalUS       uint64
		quantiles     []float64
	)
	err = conn.QueryRow(ctx, `
		SELECT countMerge(cnt), countMerge(failures), sumMerge(total_us),
		       quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur)
		  FROM transactions_5m
		 WHERE project_id = ? AND transaction = 'GET /checkout' AND environment = 'production'`,
		projectID).Scan(&cnt, &failures, &totalUS, &quantiles)
	if err != nil {
		t.Fatalf("read transactions_5m: %v", err)
	}

	if cnt != 3 {
		t.Errorf("countMerge(cnt) = %d, want 3 (MV не наполняется вставками?)", cnt)
	}
	if failures != 1 {
		t.Errorf("countMerge(failures) = %d, want 1 (считаются строки со status != 'ok')", failures)
	}
	if totalUS != 6000 {
		t.Errorf("sumMerge(total_us) = %d, want 6000", totalUS)
	}
	// quantilesState(0.5, 0.75, 0.95, 0.99) — ровно четыре уровня, в этом порядке.
	if len(quantiles) != 4 {
		t.Fatalf("quantilesMerge returned %d levels (%v), want 4", len(quantiles), quantiles)
	}
	// На [1000, 2000, 3000] ClickHouse интерполирует: p50=2000, p95=2900.
	if quantiles[0] < 1999 || quantiles[0] > 2001 {
		t.Errorf("p50 = %v, want ~2000 (levels: %v)", quantiles[0], quantiles)
	}
	if quantiles[2] < 2899 || quantiles[2] > 2901 {
		t.Errorf("p95 = %v, want ~2900 (levels: %v)", quantiles[2], quantiles)
	}
}

func TestPerformanceSchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var n int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_name = 'perf_issues'").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("table perf_issues: n=%d err=%v", n, err)
	}

	cols := map[string][]string{
		"perf_issues": {
			"id", "project_id", "fingerprint", "kind", "title", "culprit", "status",
			"count", "first_seen", "last_seen", "sample_trace_id", "evidence",
		},
		"projects":      {"transaction_sample_rate", "apdex_threshold_ms", "perf_detector_config"},
		"organizations": {"transaction_quota"},
		// 0008: отдельный счётчик транзакций — без него транзакции ели бы бюджет
		// ошибок (events_count).
		"org_usage": {"transactions_count"},
	}
	for table, names := range cols {
		for _, col := range names {
			var c int
			err := pool.QueryRow(ctx,
				`SELECT count(*) FROM information_schema.columns
				 WHERE table_name = $1 AND column_name = $2`, table, col).Scan(&c)
			if err != nil || c != 1 {
				t.Errorf("column %s.%s: n=%d err=%v", table, col, c, err)
			}
		}
	}

	// Индекс списка issue'ов проекта (§3): без него листинг перф-проблем
	// деградирует в seq scan.
	var idx int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'perf_issues' AND indexname = 'perf_issues_project_last_seen_idx'`).Scan(&idx)
	if err != nil || idx != 1 {
		t.Errorf("index perf_issues_project_last_seen_idx: n=%d err=%v", idx, err)
	}

	// Дефолты новых колонок проекта/организации — из спеки §3.
	orgID, projID := seedProject(t, ctx, pool)
	var rate float64
	var apdex int
	var cfg string
	err = pool.QueryRow(ctx,
		`SELECT transaction_sample_rate, apdex_threshold_ms, perf_detector_config
		   FROM projects WHERE id = $1`, projID).Scan(&rate, &apdex, &cfg)
	if err != nil {
		t.Fatalf("select project defaults: %v", err)
	}
	if rate != 1.0 || apdex != 300 || cfg != "{}" {
		t.Errorf("project defaults = %v/%v/%v, want 1/300/{}", rate, apdex, cfg)
	}
	var quota int64
	if err := pool.QueryRow(ctx,
		"SELECT transaction_quota FROM organizations WHERE id = $1", orgID).Scan(&quota); err != nil {
		t.Fatalf("select org quota: %v", err)
	}
	if quota != 100000 {
		t.Errorf("organizations.transaction_quota default = %d, want 100000", quota)
	}

	// (project_id, fingerprint) уникален.
	for i := 0; i < 2; i++ {
		_, err = pool.Exec(ctx,
			"INSERT INTO perf_issues (project_id, fingerprint, kind, title) VALUES ($1,'fp','n_plus_one','N+1')",
			projID)
		if i == 0 && err != nil {
			t.Fatalf("insert perf_issue: %v", err)
		}
		if i == 1 && err == nil {
			t.Error("want unique violation for duplicate (project_id, fingerprint)")
		}
	}

	// Удаление проекта каскадно уносит его perf_issues.
	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", projID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_issues WHERE project_id = $1", projID).Scan(&n); err != nil {
		t.Fatalf("count perf_issues: %v", err)
	}
	if n != 0 {
		t.Errorf("perf_issues not cascaded on project delete: n=%d", n)
	}
}

// seedProject создаёт организацию и проект, возвращая их id.
func seedProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (orgID, projID int64) {
	t.Helper()
	err := pool.QueryRow(ctx,
		"INSERT INTO organizations (name, slug, event_quota) VALUES ('perf','perf',1000) RETURNING id").Scan(&orgID)
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	err = pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, name, slug, platform) VALUES ($1,'perf','perf','go') RETURNING id",
		orgID).Scan(&projID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return orgID, projID
}

func TestTenancySchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{
		"users", "sessions", "organizations", "org_members", "org_invites",
		"teams", "team_members", "projects", "project_teams", "project_keys",
	} {
		var n int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
	// citext-уникальность email регистронезависима.
	_, err := pool.Exec(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('A@b.c','x'), ('a@B.C','y')")
	if err == nil {
		t.Error("want unique violation for case-insensitive duplicate email")
	}
}

func TestUptimeSchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{
		"monitors", "monitor_regions", "monitor_channels", "monitor_state",
		"probes", "check_queue", "incidents", "maintenance_windows",
		"status_pages", "status_page_monitors",
	} {
		var n int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
	// kind CHECK на monitors отвергает произвольные значения.
	_, err := pool.Exec(ctx,
		"INSERT INTO monitors (project_id, name, kind, interval_seconds) VALUES (1, 'x', 'bogus', 60)")
	if err == nil {
		t.Error("want CHECK violation for invalid monitors.kind")
	}
}

func TestAlertsSchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{
		"alert_rules", "alert_channels", "notification_outbox",
		"alert_throttle", "org_usage",
	} {
		var n int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
	// kind CHECK на alert_rules отвергает произвольные значения.
	_, err := pool.Exec(ctx,
		"INSERT INTO alert_rules (project_id, kind) VALUES (1, 'bogus')")
	if err == nil {
		t.Error("want CHECK violation for invalid alert_rules.kind")
	}
}
