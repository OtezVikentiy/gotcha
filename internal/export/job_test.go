package export

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestParseFormatAndExt(t *testing.T) {
	for _, c := range []struct {
		in    string
		ok    bool
		ext   string
		ctype string
	}{
		{"csv", true, "csv", "text/csv; charset=utf-8"},
		{"json", true, "json", "application/json"},
		{"ndjson", true, "ndjson", "application/x-ndjson"},
		{"xlsx", false, "", ""},
		{"", false, "", ""},
	} {
		got, ok := ParseFormat(c.in)
		if ok != c.ok {
			t.Fatalf("ParseFormat(%q): ok=%v, ожидали %v", c.in, ok, c.ok)
		}
		if !ok {
			continue
		}
		if got.Ext() != c.ext {
			t.Errorf("ParseFormat(%q).Ext() = %q, ожидали %q", c.in, got.Ext(), c.ext)
		}
		if got.ContentType() != c.ctype {
			t.Errorf("ParseFormat(%q).ContentType() = %q, ожидали %q", c.in, got.ContentType(), c.ctype)
		}
	}
}

func TestParseKindRejectsUnknown(t *testing.T) {
	if _, ok := ParseKind("traces"); ok {
		t.Fatal("ParseKind принял неизвестный вид выгрузки")
	}
	k, ok := ParseKind("events")
	if !ok || k != KindEvents {
		t.Fatalf("ParseKind(events) = %v,%v", k, ok)
	}
}

// TestContentTypeUnknownFormat — неизвестное значение Format (в обход ParseFormat,
// например уже сохранённое в базе значение из будущей версии) не должно паниковать
// и обязано откатываться на нейтральный content-type.
func TestContentTypeUnknownFormat(t *testing.T) {
	if got := Format("bogus").ContentType(); got != "application/octet-stream" {
		t.Errorf("Format(bogus).ContentType() = %q, ожидали application/octet-stream", got)
	}
}

// TestStatusTerminal — переберает все статусы: Terminal() обязан отличать
// финальные (done/failed/expired) от промежуточных (queued/running).
func TestStatusTerminal(t *testing.T) {
	for _, c := range []struct {
		status   Status
		terminal bool
	}{
		{StatusQueued, false},
		{StatusRunning, false},
		{StatusDone, true},
		{StatusFailed, true},
		{StatusExpired, true},
	} {
		if got := c.status.Terminal(); got != c.terminal {
			t.Errorf("Status(%q).Terminal() = %v, ожидали %v", c.status, got, c.terminal)
		}
	}
}

// TestMigrationCreatesExportJobs — проверяет, что миграция 0081 действительно
// накатывает таблицу заявок с ожидаемой формой, а не только сам факт её наличия:
// ключевые колонки с типами и NOT NULL, все три индекса, CHECK-констрейнты на
// перечислениях kind/format/status.
func TestMigrationCreatesExportJobs(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'export_jobs'`).Scan(&n); err != nil {
		t.Fatalf("запрос к information_schema: %v", err)
	}
	if n == 0 {
		t.Fatal("таблица export_jobs не создана миграцией")
	}

	for _, c := range []struct {
		column   string
		dataType string
		nullable string
	}{
		{"id", "bigint", "NO"},
		{"project_id", "bigint", "NO"},
		{"created_by", "bigint", "NO"},
		{"kind", "text", "NO"},
		{"format", "text", "NO"},
		{"scope_issue_id", "bigint", "YES"},
		{"params", "jsonb", "NO"},
		{"include_pii", "boolean", "NO"},
		{"status", "text", "NO"},
		{"attempts", "integer", "NO"},
		{"last_error", "text", "NO"},
		{"rows_written", "bigint", "NO"},
		{"bytes", "bigint", "NO"},
		{"truncated", "boolean", "NO"},
		{"file_ext", "text", "NO"},
		{"claimed_at", "timestamp with time zone", "YES"},
		{"created_at", "timestamp with time zone", "NO"},
		{"finished_at", "timestamp with time zone", "YES"},
		{"expires_at", "timestamp with time zone", "YES"},
	} {
		var dataType, nullable string
		err := pool.QueryRow(ctx,
			`SELECT data_type, is_nullable FROM information_schema.columns
			 WHERE table_name = 'export_jobs' AND column_name = $1`, c.column).Scan(&dataType, &nullable)
		if err != nil {
			t.Fatalf("колонка %s: %v", c.column, err)
		}
		if dataType != c.dataType {
			t.Errorf("export_jobs.%s: тип %q, ожидали %q", c.column, dataType, c.dataType)
		}
		if nullable != c.nullable {
			t.Errorf("export_jobs.%s: is_nullable=%q, ожидали %q", c.column, nullable, c.nullable)
		}
	}

	for _, idx := range []string{"export_jobs_claim_idx", "export_jobs_list_idx", "export_jobs_expire_idx"} {
		var cnt int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE tablename = 'export_jobs' AND indexname = $1`, idx).Scan(&cnt)
		if err != nil {
			t.Fatalf("индекс %s: %v", idx, err)
		}
		if cnt == 0 {
			t.Errorf("индекс %s не создан миграцией", idx)
		}
	}

	for _, c := range []struct {
		column string
		values []string
	}{
		{"kind", []string{"issues", "events"}},
		{"format", []string{"csv", "json", "ndjson"}},
		{"status", []string{"queued", "running", "done", "failed", "expired"}},
	} {
		var def string
		err := pool.QueryRow(ctx, `
			SELECT pg_get_constraintdef(con.oid)
			FROM pg_constraint con
			JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY(con.conkey)
			WHERE con.conrelid = 'export_jobs'::regclass
			  AND con.contype = 'c'
			  AND att.attname = $1`, c.column).Scan(&def)
		if err != nil {
			t.Fatalf("CHECK-констрейнт на %s: %v", c.column, err)
		}
		for _, v := range c.values {
			if !strings.Contains(def, "'"+v+"'") {
				t.Errorf("CHECK на %s %q не содержит значение %q", c.column, def, v)
			}
		}
	}
}
