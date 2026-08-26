package export

import (
	"context"
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

// TestMigrationCreatesExportJobs — проверяет, что миграция 0081 действительно
// накатывает таблицу заявок (а не только компилируется).
func TestMigrationCreatesExportJobs(t *testing.T) {
	pool := testenv.MigratedPG(t)
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'export_jobs'`).Scan(&n)
	if err != nil {
		t.Fatalf("запрос к information_schema: %v", err)
	}
	if n == 0 {
		t.Fatal("таблица export_jobs не создана миграцией")
	}
}
