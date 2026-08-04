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
	// 0007 добавляет колонку measurements.
	txDDL := showCreate("transactions")
	for _, want := range []string{
		"trace_id", "span_id", "transaction", "duration_us", "tags", "source",
		"measurements",
		"toYYYYMM(timestamp)", "ORDER BY (project_id, transaction, timestamp)", "toIntervalDay(90)",
	} {
		if !strings.Contains(txDDL, want) {
			t.Errorf("transactions DDL missing %q:\n%s", want, txDDL)
		}
	}

	// 0008: MV web_vitals_5m существует после up. Содержимое проверяется
	// поведением, см. TestWebVitals5mAggregates.
	var wvExists uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = 'web_vitals_5m'").
		Scan(&wvExists); err != nil {
		t.Fatalf("system.tables web_vitals_5m: %v", err)
	}
	if wvExists != 1 {
		t.Errorf("web_vitals_5m does not exist after up")
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

// TestMigratePGUpDownUp — up/down/up для PostgreSQL.
//
// После down проверяется, что схема ДЕЙСТВИТЕЛЬНО пуста, а не только что вызов
// вернул nil. Раньше тест смотрел лишь на err, и down-миграция, которая молча
// ничего не делает, проходила его — все 24 down-файла PG были фактически
// неверифицированы. CH-версия ниже таблицы считала, PG-версия — нет.
func TestMigratePGUpDownUp(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()

	before := userTableCount(t, ctx, pool)
	if before == 0 {
		t.Fatal("после up в public нет ни одной таблицы — up ничего не создал")
	}

	if err := db.MigrateDownPG(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	// schema_migrations заводит сам golang-migrate и down её не трогает —
	// поэтому «пусто» здесь означает «не осталось ничего, кроме неё».
	if after := userTableCount(t, ctx, pool); after != 0 {
		names := userTableNames(t, ctx, pool)
		t.Fatalf("после down осталось %d таблиц: %v — down-миграции не откатывают схему", after, names)
	}

	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if again := userTableCount(t, ctx, pool); again != before {
		t.Fatalf("повторный up дал %d таблиц вместо %d", again, before)
	}
}

// userTableCount — сколько таблиц в public, не считая служебной schema_migrations.
func userTableCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		  AND table_name <> 'schema_migrations'`).Scan(&n)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return n
}

func userTableNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		  AND table_name <> 'schema_migrations' ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// tableExistsIn/columnExistsIn — единая точка правды «есть ли таблица/колонка
// с таким именем в ЭТОЙ схеме», схема — параметр, а не литерал. Раунд правок
// 2 (находка ревью на задачу №115): тринадцать мест этого файла держали
// фильтр по table_schema каждое своей собственной копией запроса — исчезни
// фильтр из одной копии при будущей правке, ни доказательный тест (у него
// своя изолированная база без конфликта имён), ни сам пострадавший тест
// этого бы не заметил, потому что оба видят только СВОЙ экземпляр запроса.
// Хелпер даёт всем тринадцати местам ОДНУ реализацию — а значит и одну точку,
// которую способен проверить мутацией доказательный тест
// (TestInformationSchemaTableCheckIgnoresOtherSchemas ниже).
//
// Возвращают (bool, error), а не паникуют/фатализируют сами: вызывающие тесты
// расходятся в строгости — часть падает Fatalf-ом на первой пропавшей
// таблице, часть копит Errorf по всем сразу в цикле, — и это решение теста,
// не хелпера; учитывая любую степень строгости здесь означало бы либо менять
// поведение мест, которые ничего не просили менять, либо тащить в хелпер
// параметр «насколько падать», что хуже простого возврата ошибки.
func tableExistsIn(ctx context.Context, pool *pgxpool.Pool, schema, table string) (bool, error) {
	var n int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
		schema, table).Scan(&n)
	return n == 1, err
}

func columnExistsIn(ctx context.Context, pool *pgxpool.Pool, schema, table, column string) (bool, error) {
	var n int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 AND column_name = $3",
		schema, table, column).Scan(&n)
	return n == 1, err
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
	for _, table := range []string{"events", "check_results", "transactions", "spans", "transactions_5m", "web_vitals_5m"} {
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

// TestWebVitals5mAggregates закрепляет MV web_vitals_5m поведением: вставляем
// транзакции с measurements['lcp'] и читаем p75 через quantilesMerge. Ключевое
// свойство — mapContains-фильтр: транзакция без lcp не считается нулём и не
// попадает в квантиль/счётчик.
func TestWebVitals5mAggregates(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const projectID uint64 = 77
	ts := time.Date(2026, 7, 15, 9, 2, 30, 0, time.UTC) // bucket 09:00
	rows := []map[string]float64{
		{"lcp": 2000, "inp": 150},
		{"lcp": 2400},
		{"lcp": 2600},
		{}, // без lcp: не должен попасть ни в квантиль, ни в lcp_count
	}

	batch, err := conn.PrepareBatch(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, op, timestamp, duration_us, status, environment, measurements)")
	if err != nil {
		t.Fatalf("PrepareBatch transactions: %v", err)
	}
	for i, m := range rows {
		err := batch.Append(projectID, "trace", "span", "GET /home", "pageload",
			ts.Add(time.Duration(i)*time.Second), uint32(1000), "ok", "production", m)
		if err != nil {
			t.Fatalf("append row %d: %v", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send transactions batch: %v", err)
	}

	var (
		lcpCount  uint64
		inpCount  uint64
		quantiles []float64
	)
	err = conn.QueryRow(ctx, `
		SELECT countMerge(lcp_count), countMerge(inp_count), quantilesMerge(0.75)(lcp)
		  FROM web_vitals_5m
		 WHERE project_id = ? AND transaction = 'GET /home' AND environment = 'production'`,
		projectID).Scan(&lcpCount, &inpCount, &quantiles)
	if err != nil {
		t.Fatalf("read web_vitals_5m: %v", err)
	}

	// Отсутствующий lcp (mapContains guard) не считается — три присутствующих.
	if lcpCount != 3 {
		t.Errorf("countMerge(lcp_count) = %d, want 3 (mapContains не отсекает отсутствующий lcp?)", lcpCount)
	}
	// Пер-vital счётчики: inp есть только у одной строки (нужен плану 3 для min_samples).
	if inpCount != 1 {
		t.Errorf("countMerge(inp_count) = %d, want 1", inpCount)
	}
	// quantilesStateIf(0.75) — ровно один уровень.
	if len(quantiles) != 1 {
		t.Fatalf("quantilesMerge returned %d levels (%v), want 1", len(quantiles), quantiles)
	}
	// p75 из [2000, 2400, 2600] интерполируется в ~2500; если бы отсутствующий
	// lcp считался нулём, p75 просел бы.
	if quantiles[0] < 2499 || quantiles[0] > 2501 {
		t.Errorf("p75(lcp) = %v, want ~2500 (levels: %v)", quantiles[0], quantiles)
	}
}

func TestPerformanceSchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ok, err := tableExistsIn(ctx, pool, "public", "perf_issues")
	if err != nil || !ok {
		t.Fatalf("table perf_issues: exists=%v err=%v", ok, err)
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
			ok, err := columnExistsIn(ctx, pool, "public", table, col)
			if err != nil || !ok {
				t.Errorf("column %s.%s: exists=%v err=%v", table, col, ok, err)
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
	// Миграция 0018 (PROD-B2) сменила дефолт на 0 (безлимит) для OSS-позиционирования.
	if quota != 0 {
		t.Errorf("organizations.transaction_quota default = %d, want 0", quota)
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
	var n int
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

func TestRegressionSchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ok, err := tableExistsIn(ctx, pool, "public", "perf_regressions")
	if err != nil || !ok {
		t.Fatalf("table perf_regressions: exists=%v err=%v", ok, err)
	}

	cols := map[string][]string{
		"perf_regressions": {
			"id", "project_id", "target_kind", "target", "metric", "status",
			"baseline_value", "peak_value", "current_value", "started_at",
			"resolved_at", "notified_open", "notified_close",
		},
		"projects": {"perf_regression_config"},
	}
	for table, names := range cols {
		for _, col := range names {
			ok, err := columnExistsIn(ctx, pool, "public", table, col)
			if err != nil || !ok {
				t.Errorf("column %s.%s: exists=%v err=%v", table, col, ok, err)
			}
		}
	}

	// Оба индекса на месте: частичный уникальный на открытые инциденты и
	// индекс списка регрессий проекта.
	for _, idx := range []string{
		"perf_regressions_one_open_idx", "perf_regressions_project_started_idx",
	} {
		var c int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE tablename = 'perf_regressions' AND indexname = $1`, idx).Scan(&c)
		if err != nil || c != 1 {
			t.Errorf("index %s: n=%d err=%v", idx, c, err)
		}
	}

	// Дефолт новой колонки проекта — '{}'.
	_, projID := seedProject(t, ctx, pool)
	var cfg string
	if err := pool.QueryRow(ctx,
		"SELECT perf_regression_config FROM projects WHERE id = $1", projID).Scan(&cfg); err != nil {
		t.Fatalf("select perf_regression_config: %v", err)
	}
	if cfg != "{}" {
		t.Errorf("perf_regression_config default = %q, want {}", cfg)
	}

	insertOpen := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO perf_regressions
			   (project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
			 VALUES ($1,'endpoint_p95','GET /checkout','duration',100,200,180)`, projID)
		return err
	}
	// Частичный уникальный индекс: второй ОТКРЫТЫЙ инцидент по той же
	// (project_id, target, metric) недопустим.
	if err := insertOpen(); err != nil {
		t.Fatalf("insert first open regression: %v", err)
	}
	if err := insertOpen(); err == nil {
		t.Error("want unique violation for second open regression on same target")
	}
	// После закрытия первого можно открыть новый — индекс частичный (WHERE
	// status='open').
	if _, err := pool.Exec(ctx,
		"UPDATE perf_regressions SET status='resolved', resolved_at=now() WHERE project_id=$1", projID); err != nil {
		t.Fatalf("resolve regression: %v", err)
	}
	if err := insertOpen(); err != nil {
		t.Errorf("insert open regression after resolving previous: %v", err)
	}

	// status CHECK отвергает произвольные значения.
	_, err = pool.Exec(ctx,
		`INSERT INTO perf_regressions
		   (project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value)
		 VALUES ($1,'endpoint_p95','GET /x','duration','bogus',1,1,1)`, projID)
	if err == nil {
		t.Error("want CHECK violation for invalid perf_regressions.status")
	}

	// Удаление проекта каскадно уносит его perf_regressions.
	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", projID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_regressions WHERE project_id = $1", projID).Scan(&n); err != nil {
		t.Fatalf("count perf_regressions: %v", err)
	}
	if n != 0 {
		t.Errorf("perf_regressions not cascaded on project delete: n=%d", n)
	}
}

func TestTenancySchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{
		"users", "sessions", "organizations", "org_members", "org_invites",
		"teams", "team_members", "projects", "project_teams", "project_keys",
	} {
		ok, err := tableExistsIn(ctx, pool, "public", table)
		if err != nil || !ok {
			t.Errorf("table %s: exists=%v err=%v", table, ok, err)
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
		ok, err := tableExistsIn(ctx, pool, "public", table)
		if err != nil || !ok {
			t.Errorf("table %s: exists=%v err=%v", table, ok, err)
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
		ok, err := tableExistsIn(ctx, pool, "public", table)
		if err != nil || !ok {
			t.Errorf("table %s: exists=%v err=%v", table, ok, err)
		}
	}
	// kind CHECK на alert_rules отвергает произвольные значения.
	_, err := pool.Exec(ctx,
		"INSERT INTO alert_rules (project_id, kind) VALUES (1, 'bogus')")
	if err == nil {
		t.Error("want CHECK violation for invalid alert_rules.kind")
	}
}

func TestMigrate0012OAuthIdentities(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	// password_hash должен быть nullable.
	var isNullable string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'password_hash'`).Scan(&isNullable); err != nil {
		t.Fatalf("query column: %v", err)
	}
	if isNullable != "YES" {
		t.Fatalf("password_hash is_nullable = %q, want YES", isNullable)
	}

	// Вставка юзера без пароля и его личности проходит.
	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ('oauth-only@example.com') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("insert passwordless user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO user_identities (user_id, provider, subject, email) VALUES ($1,'oidc','sub-1','oauth-only@example.com')", uid); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	// Тот же (provider, subject) второй раз → нарушение PK.
	if _, err := pool.Exec(ctx,
		"INSERT INTO user_identities (user_id, provider, subject) VALUES ($1,'oidc','sub-1')", uid); err == nil {
		t.Fatal("duplicate (provider,subject) must violate PK")
	}
}

func TestMigrateMetricSchemaPG(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	if ok, err := columnExistsIn(ctx, pool, "public", "org_usage", "metrics_count"); err != nil || !ok {
		t.Fatalf("metrics_count column: exists=%v err=%v", ok, err)
	}
	for _, tbl := range []string{"metric_alert_rules", "metric_incidents"} {
		if ok, err := tableExistsIn(ctx, pool, "public", tbl); err != nil || !ok {
			t.Fatalf("table %s: exists=%v err=%v", tbl, ok, err)
		}
	}
	// CHECK на aggregation отвергает мусор.
	if _, err := pool.Exec(ctx,
		"INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold) VALUES (1,'m','bogus','gt',1)"); err == nil {
		t.Error("want CHECK violation for invalid aggregation")
	}
}

func TestMigrateMetricPointsCH(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	if err := conn.Exec(ctx,
		"INSERT INTO metric_points (project_id,name,type,ts,value) VALUES (1,'m','gauge',now64(3),1.5)"); err != nil {
		t.Fatalf("insert metric_points: %v", err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM metric_points WHERE project_id=1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err=%v", n, err)
	}
	// Ретенция идемпотентна и переопределяет TTL.
	if err := db.ApplyMetricRetention(ctx, conn, 7); err != nil {
		t.Fatalf("ApplyMetricRetention: %v", err)
	}
	if err := db.ApplyMetricRetention(ctx, conn, 7); err != nil {
		t.Fatalf("ApplyMetricRetention (idempotent): %v", err)
	}
}

func TestMigrateProfileSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	if ok, err := columnExistsIn(ctx, pool, "public", "org_usage", "profiles_count"); err != nil || !ok {
		t.Fatalf("profiles_count column: exists=%v err=%v", ok, err)
	}
	conn := testenv.MigratedCH(t)
	if err := conn.Exec(ctx,
		"INSERT INTO profile_samples (project_id,profile_type,ts,stack,value) VALUES (1,'cpu',now64(3),['a','b'],5)"); err != nil {
		t.Fatalf("insert profile_samples: %v", err)
	}
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM profile_samples WHERE project_id=1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err=%v", n, err)
	}
	if err := db.ApplyProfileRetention(ctx, conn, 3); err != nil {
		t.Fatalf("retention: %v", err)
	}
	if err := db.ApplyProfileRetention(ctx, conn, 3); err != nil {
		t.Fatalf("retention idempotent: %v", err)
	}
}

func TestMigrateProfileTraceID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	if err := conn.Exec(ctx,
		"INSERT INTO profile_samples (project_id,profile_type,ts,stack,value,trace_id) VALUES (1,'cpu',now64(3),['a'],5,'tr-1')"); err != nil {
		t.Fatalf("insert with trace_id: %v", err)
	}
	var tid string
	if err := conn.QueryRow(ctx, "SELECT trace_id FROM profile_samples WHERE project_id=1 LIMIT 1").Scan(&tid); err != nil || tid != "tr-1" {
		t.Fatalf("trace_id = %q err=%v", tid, err)
	}
}

func TestMigrateProfileRegressions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	if ok, err := tableExistsIn(ctx, pool, "public", "profile_regressions"); err != nil || !ok {
		t.Fatalf("table missing: exists=%v err=%v", ok, err)
	}
	// Нужен проект для FK.
	var uid, orgID, projID int64
	pool.QueryRow(ctx, "INSERT INTO users (email,password_hash) VALUES ('pr@e.com','x') RETURNING id").Scan(&uid)
	pool.QueryRow(ctx, "INSERT INTO organizations (slug,name,event_quota) VALUES ('pr','pr',1000000) RETURNING id").Scan(&orgID)
	pool.QueryRow(ctx, "INSERT INTO projects (org_id,slug,name,platform) VALUES ($1,'pr','pr','go') RETURNING id", orgID).Scan(&projID)

	ins := "INSERT INTO profile_regressions (project_id,service,profile_type,function,baseline_share,peak_share,current_share) VALUES ($1,'api','cpu','slow',0.05,0.2,0.2)"
	if _, err := pool.Exec(ctx, ins, projID); err != nil {
		t.Fatalf("insert open: %v", err)
	}
	// Второй открытый на ту же функцию → нарушение partial-индекса.
	if _, err := pool.Exec(ctx, ins, projID); err == nil {
		t.Fatal("want one-open unique violation")
	}
	// CHECK status.
	if _, err := pool.Exec(ctx,
		"INSERT INTO profile_regressions (project_id,service,profile_type,function,status,baseline_share,peak_share,current_share) VALUES ($1,'api','cpu','x','bogus',0,0,0)", projID); err == nil {
		t.Fatal("want status CHECK violation")
	}
}

// TestMigrateEventQuotaDefault — RA-6: после 0020 дефолт колонки event_quota — 0
// (OSS-безлимит), как у tx/metric/profile-квот в 0018. Орг, вставленный без явного
// event_quota, получает 0, а не legacy-хардкод 1000000.
func TestMigrateEventQuotaDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name) VALUES ('eq', 'eq') RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("insert org without event_quota: %v", err)
	}
	var quota int64
	if err := pool.QueryRow(ctx,
		"SELECT event_quota FROM organizations WHERE id = $1", orgID).Scan(&quota); err != nil {
		t.Fatalf("select event_quota: %v", err)
	}
	if quota != 0 {
		t.Errorf("organizations.event_quota default = %d, want 0", quota)
	}
}

func TestMigrateOrgSSO(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	if ok, err := tableExistsIn(ctx, pool, "public", "org_sso"); err != nil || !ok {
		t.Fatalf("org_sso missing: exists=%v err=%v", ok, err)
	}
	mkOrg := func(slug string) int64 {
		var uid, orgID int64
		pool.QueryRow(ctx, "INSERT INTO users (email,password_hash) VALUES ($1,'x') RETURNING id", slug+"@e.com").Scan(&uid)
		pool.QueryRow(ctx, "INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,1000000) RETURNING id", slug).Scan(&orgID)
		return orgID
	}
	o1, o2 := mkOrg("sso1"), mkOrg("sso2")
	ins := "INSERT INTO org_sso (org_id,issuer,client_id,client_secret,domain) VALUES ($1,'https://i','c','s',$2)"
	if _, err := pool.Exec(ctx, ins, o1, "corp.com"); err != nil {
		t.Fatalf("insert o1: %v", err)
	}
	// Тот же домен у другого орга → нарушение UNIQUE.
	if _, err := pool.Exec(ctx, ins, o2, "corp.com"); err == nil {
		t.Fatal("want domain UNIQUE violation")
	}
	// CHECK default_role.
	if _, err := pool.Exec(ctx,
		"INSERT INTO org_sso (org_id,issuer,client_id,client_secret,domain,default_role) VALUES ($1,'https://i','c','s','x.com','owner')", o2); err == nil {
		t.Fatal("want default_role CHECK violation")
	}
}

// TestInformationSchemaTableCheckIgnoresOtherSchemas — находка №115: все
// "таблица создана"/"колонка есть"-проверки в этом файле шли через
// information_schema.tables и information_schema.columns БЕЗ фильтра по
// table_schema, поэтому одноимённая таблица (и её колонки) в чужой схеме
// считались бы наравне с настоящей, и проверка могла пройти по подделке, а
// не по продуктовой таблице.
//
// Раунд правок 2 (см. task-10-report.md): все тринадцать мест переведены на
// общие tableExistsIn/columnExistsIn (см. их докблок выше), и этот тест
// теперь бьёт НЕ по копии запроса внутри себя, а по самим хелперам — иначе
// он доказывал бы, что приём в принципе работает, но не охранял бы ни одно
// из тринадцати мест, которые от него зависят.
//
// Запрос с фильтром и без него на ЧИСТОЙ базе дают ОДИНАКОВЫЙ результат —
// обычный прогон остальных тестов файла правку не проверяет вообще, нужен
// сценарий с реальным конфликтом имён. Схема decoy заводит ДВЕ асимметричные
// подсадные цели:
//   - ghost_table — имени с таким названием в public нет вовсе, только в
//     decoy. Если бы tableExistsIn не фильтровал по схеме, запрос про
//     public.ghost_table всё равно нашёл бы decoy.ghost_table и ошибочно
//     подтвердил бы её существование в public.
//   - org_sso с колонкой decoy_marker — совпадает по ИМЕНИ ТАБЛИЦЫ с
//     настоящей public.org_sso (ровно тот сценарий, который находка №115
//     называет реальным риском), но decoy_marker у настоящей таблицы
//     заведомо нет (сверено по 0016_org_sso.up.sql:
//     org_id/issuer/client_id/client_secret/domain/default_role/enforced/
//     created_at) — так разница между «нашли у настоящей» и «нашли у чужой»
//     видна однозначно, а не по случайному совпадению имён колонок.
func TestInformationSchemaTableCheckIgnoresOtherSchemas(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "CREATE SCHEMA decoy"); err != nil {
		t.Fatalf("create schema decoy: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE decoy.ghost_table (id int)"); err != nil {
		t.Fatalf("create decoy.ghost_table: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE decoy.org_sso (id int, decoy_marker text)"); err != nil {
		t.Fatalf("create decoy.org_sso: %v", err)
	}

	// (1) tableExistsIn: подготовка сценария — ghost_table действительно
	// существует, просто не в public.
	if ok, err := tableExistsIn(ctx, pool, "decoy", "ghost_table"); err != nil || !ok {
		t.Fatalf("подготовка сценария сломана: decoy.ghost_table не найден хелпером (exists=%v err=%v) — сценарий не воспроизводит находку №115", ok, err)
	}
	// Сама проверка: запрос про public.ghost_table обязан вернуть false —
	// таблица есть в базе, но не в этой схеме. Если бы фильтр не работал,
	// хелпер нашёл бы её через decoy и ошибочно ответил true.
	if ok, err := tableExistsIn(ctx, pool, "public", "ghost_table"); err != nil || ok {
		t.Fatalf("tableExistsIn не фильтрует по схеме: public.ghost_table сообщена найденной (exists=%v err=%v), хотя такая таблица есть только в decoy", ok, err)
	}
	// И настоящая public.org_sso по-прежнему находится, несмотря на
	// одноимённую decoy.org_sso рядом.
	if ok, err := tableExistsIn(ctx, pool, "public", "org_sso"); err != nil || !ok {
		t.Fatalf("tableExistsIn потерял настоящую public.org_sso: exists=%v err=%v", ok, err)
	}

	// (2) columnExistsIn: подготовка сценария — decoy_marker есть у
	// decoy.org_sso.
	if ok, err := columnExistsIn(ctx, pool, "decoy", "org_sso", "decoy_marker"); err != nil || !ok {
		t.Fatalf("подготовка сценария сломана: decoy.org_sso.decoy_marker не найдена хелпером (exists=%v err=%v)", ok, err)
	}
	// Сама проверка: у настоящей public.org_sso decoy_marker быть не должно.
	// Если бы фильтр не работал, хелпер нашёл бы её через decoy.org_sso и
	// ошибочно подтвердил бы существование поля, которого в реальной
	// таблице нет.
	if ok, err := columnExistsIn(ctx, pool, "public", "org_sso", "decoy_marker"); err != nil || ok {
		t.Fatalf("columnExistsIn не фильтрует по схеме: public.org_sso.decoy_marker сообщена найденной (exists=%v err=%v), хотя у настоящей таблицы такой колонки нет", ok, err)
	}
}

// TestTransactionRetention: ApplyTransactionRetention выставляет настраиваемый
// TTL на transactions (по колонке timestamp) и на MV transactions_5m (по
// колонке bucket). Идемпотентна: повторный прогон не ошибка.
func TestTransactionRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.ApplyTransactionRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplyTransactionRetention: %v", err)
	}
	// Повторный прогон идемпотентен (needsRetention видит уже выставленный TTL).
	if err := db.ApplyTransactionRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplyTransactionRetention (idempotent): %v", err)
	}

	var txDDL string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE transactions").Scan(&txDDL); err != nil {
		t.Fatalf("SHOW CREATE TABLE transactions: %v", err)
	}
	if !strings.Contains(txDDL, "toIntervalDay(30)") {
		t.Errorf("transactions DDL без toIntervalDay(30):\n%s", txDDL)
	}

	// transactions_5m — MATERIALIZED VIEW без TO-таблицы: TTL живёт на скрытой
	// storage-таблице .inner_id.<uuid>, а SHOW CREATE TABLE самой вьюхи его не
	// показывает. Поэтому проверяем TTL по внутренней таблице.
	var inner string
	if err := conn.QueryRow(ctx,
		"SELECT concat('.inner_id.', toString(uuid)) FROM system.tables "+
			"WHERE database = currentDatabase() AND name = 'transactions_5m'").Scan(&inner); err != nil {
		t.Fatalf("resolve transactions_5m inner table: %v", err)
	}
	var mvDDL string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+inner+"`").Scan(&mvDDL); err != nil {
		t.Fatalf("SHOW CREATE TABLE %s: %v", inner, err)
	}
	if !strings.Contains(mvDDL, "toIntervalDay(30)") {
		t.Errorf("transactions_5m inner DDL без toIntervalDay(30):\n%s", mvDDL)
	}
	// TTL у 5m должен считаться от bucket, а не от timestamp.
	if !strings.Contains(mvDDL, "TTL bucket") {
		t.Errorf("transactions_5m TTL не по колонке bucket:\n%s", mvDDL)
	}
}

// TestWebVitalsRetention: ApplyWebVitalsRetention выставляет настраиваемый TTL на
// MV web_vitals_5m (по колонке bucket её скрытой storage-таблицы). Идемпотентна.
func TestWebVitalsRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.ApplyWebVitalsRetention(ctx, conn, 45); err != nil {
		t.Fatalf("ApplyWebVitalsRetention: %v", err)
	}
	// Повторный прогон идемпотентен (needsRetention видит уже выставленный TTL).
	if err := db.ApplyWebVitalsRetention(ctx, conn, 45); err != nil {
		t.Fatalf("ApplyWebVitalsRetention (idempotent): %v", err)
	}

	// web_vitals_5m — MATERIALIZED VIEW без TO-таблицы: TTL живёт на скрытой
	// storage-таблице .inner_id.<uuid>. Проверяем TTL по внутренней таблице.
	var inner string
	if err := conn.QueryRow(ctx,
		"SELECT concat('.inner_id.', toString(uuid)) FROM system.tables "+
			"WHERE database = currentDatabase() AND name = 'web_vitals_5m'").Scan(&inner); err != nil {
		t.Fatalf("resolve web_vitals_5m inner table: %v", err)
	}
	var mvDDL string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+inner+"`").Scan(&mvDDL); err != nil {
		t.Fatalf("SHOW CREATE TABLE %s: %v", inner, err)
	}
	if !strings.Contains(mvDDL, "toIntervalDay(45)") {
		t.Errorf("web_vitals_5m inner DDL без toIntervalDay(45):\n%s", mvDDL)
	}
	// TTL у 5m должен считаться от bucket, а не от timestamp.
	if !strings.Contains(mvDDL, "TTL bucket") {
		t.Errorf("web_vitals_5m TTL не по колонке bucket:\n%s", mvDDL)
	}
}

// TestSchemaVersionAndCheck: после полной миграции SchemaVersion возвращает
// встроенный максимум и dirty=false, а CheckSchemaCurrent проходит без ошибки
// (RA-8: гейт для AUTO_MIGRATE=false).
func TestSchemaVersionAndCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	dsn := testenv.PostgresDSN(t)

	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG: %v", err)
	}

	ver, dirty, err := db.SchemaVersion(dsn)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if dirty {
		t.Errorf("SchemaVersion dirty = true после успешной миграции")
	}
	if ver == 0 {
		t.Errorf("SchemaVersion = 0 после применения всех миграций")
	}

	// После полной миграции схема не отстаёт — гейт пропускает старт.
	pool := testenv.MigratedPG(t)
	if err := db.CheckSchemaCurrent(context.Background(), pool, dsn); err != nil {
		t.Errorf("CheckSchemaCurrent после полной миграции: %v", err)
	}
}

// TestSchemaVersionAndCheckCH: CH-аналог TestSchemaVersionAndCheck (audit3).
// После полной CH-миграции CheckSchemaCurrentCH проходит без ошибки — гейт для
// AUTO_MIGRATE=false и на ClickHouse (RA-8 закрыл только PG).
func TestSchemaVersionAndCheckCH(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	dsn := testenv.ClickHouseDSN(t)

	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}

	// После полной миграции CH-схема не отстаёт и не впереди — гейт пропускает старт.
	pool := testenv.MigratedPG(t)
	if err := db.CheckSchemaCurrentCH(context.Background(), pool, dsn); err != nil {
		t.Errorf("CheckSchemaCurrentCH после полной миграции: %v", err)
	}
}

// №34: days=0 снимает TTL (REMOVE TTL) вместо MODIFY TTL + INTERVAL 0 DAY,
// который удалил бы все данные немедленно. Идемпотентно в обе стороны:
// повторное снятие с таблицы без TTL — no-op, после снятия TTL ставится
// заново. В конце тест возвращает TTL к значениям миграций (90/30), чтобы
// не влиять на соседние тесты, работающие с той же базой.
func TestRetentionZeroRemovesTTL(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()
	showCreate := func(table string) string {
		var ddl string
		if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+table+"`").Scan(&ddl); err != nil {
			t.Fatalf("SHOW CREATE TABLE %s: %v", table, err)
		}
		return ddl
	}

	if err := db.ApplyRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplyRetention(30): %v", err)
	}
	if err := db.ApplyRetention(ctx, conn, 0); err != nil {
		t.Fatalf("ApplyRetention(0): %v", err)
	}
	for _, table := range []string{"events", "check_results"} {
		if ddl := showCreate(table); strings.Contains(ddl, "TTL ") {
			t.Errorf("%s: TTL survived removal:\n%s", table, ddl)
		}
	}
	// Идемпотентность: снятие TTL с таблицы без TTL — no-op без ошибки.
	if err := db.ApplyRetention(ctx, conn, 0); err != nil {
		t.Fatalf("ApplyRetention(0) second run: %v", err)
	}
	// Обратный путь: TTL ставится заново после снятия.
	if err := db.ApplyRetention(ctx, conn, 90); err != nil {
		t.Fatalf("ApplyRetention(90) restore: %v", err)
	}
	if ddl := showCreate("events"); !strings.Contains(ddl, "toIntervalDay(90)") {
		t.Errorf("events: TTL not restored:\n%s", ddl)
	}

	// MV: ноль снимает TTL и с transactions, и с inner-таблицы transactions_5m.
	if err := db.ApplyTransactionRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplyTransactionRetention(30): %v", err)
	}
	if err := db.ApplyTransactionRetention(ctx, conn, 0); err != nil {
		t.Fatalf("ApplyTransactionRetention(0): %v", err)
	}
	if ddl := showCreate("transactions"); strings.Contains(ddl, "TTL ") {
		t.Errorf("transactions: TTL survived removal:\n%s", ddl)
	}
	var uuid string
	if err := conn.QueryRow(ctx,
		"SELECT toString(uuid) FROM system.tables WHERE database = currentDatabase() AND name = 'transactions_5m'").
		Scan(&uuid); err != nil {
		t.Fatalf("resolve transactions_5m uuid: %v", err)
	}
	if ddl := showCreate(".inner_id." + uuid); strings.Contains(ddl, "TTL ") {
		t.Errorf("transactions_5m inner: TTL survived removal:\n%s", ddl)
	}
	if err := db.ApplyTransactionRetention(ctx, conn, 30); err != nil {
		t.Fatalf("ApplyTransactionRetention(30) restore: %v", err)
	}
}
