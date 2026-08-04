package templates

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestPerfIssueTitle — заголовок perf-находки строится по локали смотрящего
// из kind+description (№132): у http_flood параметром служит culprit, старые
// строки без извлечённого параметра показывают сохранённый title как есть.
func TestPerfIssueTitle(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	cases := []struct {
		name   string
		iss    trace.PerfIssue
		ruWant string
		enWant string
	}{
		{
			"n+1",
			trace.PerfIssue{Kind: trace.KindNPlusOne, Description: "SELECT * FROM x", Culprit: "GET /a"},
			"N+1 запросов: SELECT * FROM x", "N+1 queries: SELECT * FROM x",
		},
		{
			"slow",
			trace.PerfIssue{Kind: trace.KindSlowDBQuery, Description: "SELECT 1", Culprit: "GET /a"},
			"Медленный запрос: SELECT 1", "Slow query: SELECT 1",
		},
		{
			"flood",
			trace.PerfIssue{Kind: trace.KindHTTPFlood, Culprit: "GET /orders"},
			"Лавина HTTP-вызовов: GET /orders", "HTTP call flood: GET /orders",
		},
		{
			"legacy",
			trace.PerfIssue{Kind: trace.KindNPlusOne, Title: "N+1 запросов: старая строка", Culprit: "GET /a"},
			"N+1 запросов: старая строка", "N+1 запросов: старая строка",
		},
		{
			"empty",
			trace.PerfIssue{Kind: trace.KindSlowDBQuery},
			"Медленный запрос", "Slow query",
		},
	}
	for _, c := range cases {
		if got := perfIssueTitle(ru, c.iss); got != c.ruWant {
			t.Errorf("%s ru = %q, want %q", c.name, got, c.ruWant)
		}
		if got := perfIssueTitle(en, c.iss); got != c.enWant {
			t.Errorf("%s en = %q, want %q", c.name, got, c.enWant)
		}
	}
}
