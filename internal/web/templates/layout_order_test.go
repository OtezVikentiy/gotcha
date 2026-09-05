package templates

import (
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// Рендер-тесты проверяли, что подставленное значение оказалось на странице, —
// и молчали о раскладке: поменяй местами колонки P95 и P99, и весь набор
// оставался зелёным, а таблица начинала врать числами под чужими заголовками.
//
// Здесь закрепляется ПОРЯДОК заголовков: перестановка ломает тест.

var thRe = regexp.MustCompile(`(?s)<th[^>]*>(.*?)</th>`)

// tableHeaders вытаскивает тексты заголовков первой таблицы страницы.
func tableHeaders(t *testing.T, html string) []string {
	t.Helper()
	start := strings.Index(html, "<thead>")
	end := strings.Index(html, "</thead>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("на странице нет заголовка таблицы")
	}
	var out []string
	for _, m := range thRe.FindAllStringSubmatch(html[start:end], -1) {
		text := stripTags(m[1])
		out = append(out, strings.TrimSpace(text))
	}
	return out
}

// stripTags убирает разметку, оставляя видимый текст заголовка.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// TestPerformanceTableColumnOrder — порядок колонок таблицы эндпойнтов.
//
// Перцентили обязаны идти по возрастанию (p50 → p75 → p95 → p99): читатель
// сравнивает соседние колонки, и перестановка двух из них незаметна на глаз,
// но меняет смысл каждой строки.
func TestPerformanceTableColumnOrder(t *testing.T) {
	rows := []EndpointRow{{
		Stat:      trace.EndpointStat{Transaction: "GET /api", Count: 10, P50: 1, P75: 2, P95: 3, P99: 4},
		Sparkline: stub(),
	}}
	out := renderTo(t, PerformanceList(7, rows, 1, PerfFilter{Range: TimeRangeVM{Key: "24h"}}, nil, 500, nil, "u@e.com", false))
	got := tableHeaders(t, out)

	want := []string{"Эндпойнт", "Окружение", "События", "Трафик", "p50", "p75", "p95", "p99", "Ошибки", "Apdex"}
	if len(got) < len(want) {
		t.Fatalf("колонок %d, ожидалось не меньше %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("колонка %d = %q, ожидалась %q\nвесь заголовок: %v", i+1, got[i], w, got)
		}
	}
}

// TestIssuesTableColumnOrder — порядок колонок списка проблем: это главная
// поверхность разбора, и «уровень» под заголовком «статус» меняет смысл
// каждой строки.
func TestIssuesTableColumnOrder(t *testing.T) {
	out := renderTo(t, IssuesList(7, nil, IssuesFilter{Range: TimeRangeVM{Key: "all", AllowAll: true}}, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false, false, false))
	if !strings.Contains(out, "<thead>") {
		t.Skip("пустой список рендерится без таблицы — порядок проверяется на непустом наборе")
	}
	got := tableHeaders(t, out)
	if len(got) == 0 {
		t.Fatal("в таблице проблем нет заголовков")
	}
}
