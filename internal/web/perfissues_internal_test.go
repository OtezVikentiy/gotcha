package web

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func TestPerfEvidenceSpanIDs(t *testing.T) {
	if got := perfEvidenceSpanIDs([]byte(`{"count":9,"span_ids":["a","b"]}`)); len(got) != 2 || got[0] != "a" {
		t.Errorf("span_ids = %v", got)
	}
	// нет ключа / битый / пустой → nil
	for _, in := range []string{`{"count":9}`, `not-json`, ``} {
		if got := perfEvidenceSpanIDs([]byte(in)); got != nil {
			t.Errorf("perfEvidenceSpanIDs(%q) = %v, want nil", in, got)
		}
	}
}

func TestCodeLocFromData(t *testing.T) {
	code := codeLocFromData(map[string]string{
		"code.filepath": "app/pay.py", "code.lineno": "42", "code.function": "reconcile",
	})
	if code == nil || code.File != "app/pay.py" || code.Line != "42" || code.Function != "reconcile" {
		t.Fatalf("code = %+v", code)
	}
	// только функция (без файла) — тоже показываем
	if c := codeLocFromData(map[string]string{"code.function": "f"}); c == nil || c.Function != "f" {
		t.Errorf("function-only should yield a loc: %+v", c)
	}
	// ни файла, ни функции, и пустая мапа → nil (показывать нечего)
	if c := codeLocFromData(map[string]string{"db.system": "postgresql"}); c != nil {
		t.Errorf("no file/function → want nil, got %+v", c)
	}
	if c := codeLocFromData(nil); c != nil {
		t.Errorf("nil data → want nil, got %+v", c)
	}
}

func TestEnrichPerfDetail(t *testing.T) {
	// Берём первый спан с непустым описанием (спаны уже по длительности убыв.).
	var d templates.PerfIssueDetailData
	enrichPerfDetail(&d, []trace.SpanDetail{
		{SpanID: "s1", Op: "view", Description: "", DurationUS: 8000},
		{SpanID: "s2", Op: "db.sql.query", Description: "SELECT 1", DurationUS: 6000,
			Data: map[string]string{"db.system": "postgresql", "code.filepath": "a.py", "code.lineno": "7", "code.function": "g"}},
	})
	if d.Query != "SELECT 1" || d.QueryOp != "db.sql.query" || d.SpanDurationUS != 6000 {
		t.Errorf("query fields = %q %q %d", d.Query, d.QueryOp, d.SpanDurationUS)
	}
	if d.DBSystem != "postgresql" || d.Code == nil || d.Code.File != "a.py" {
		t.Errorf("code/db = %q %+v", d.DBSystem, d.Code)
	}

	// Нет спанов — ничего не проставляется.
	var empty templates.PerfIssueDetailData
	enrichPerfDetail(&empty, nil)
	if empty.Query != "" || empty.Code != nil {
		t.Errorf("empty spans should leave detail bare: %+v", empty)
	}
}
