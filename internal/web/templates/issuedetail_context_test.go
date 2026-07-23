package templates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// contextGroups парсит contexts-JSON в группы, сортирует по ключу и разворачивает
// вложенность точечными ключами.
func TestContextGroups(t *testing.T) {
	js := `{"os":{"name":"Linux","version":"4.18"},"runtime":{"name":"php","version":"8.4"},"trace":{"op":"http.server","data":{"http.url":"https://x/y","route":"r"}}}`
	groups := contextGroups(js)
	if len(groups) != 3 {
		t.Fatalf("групп=%d, want 3", len(groups))
	}

	find := func(title string) *ctxGroup {
		for i := range groups {
			if groups[i].Title == title {
				return &groups[i]
			}
		}
		return nil
	}

	os := find("OS")
	if os == nil {
		t.Fatal("нет группы OS")
	}
	got := map[string]string{}
	for _, r := range os.Rows {
		got[r.Key] = r.Val
	}
	if got["name"] != "Linux" || got["version"] != "4.18" {
		t.Fatalf("os rows = %v", got)
	}

	trace := find("Trace")
	if trace == nil {
		t.Fatal("нет группы Trace")
	}
	hasURL := false
	for _, r := range trace.Rows {
		if r.Key == "data.http.url" && r.Val == "https://x/y" {
			hasURL = true
		}
	}
	if !hasURL {
		t.Fatalf("вложенность не развёрнута (нет data.http.url): %v", trace.Rows)
	}
}

func TestContextGroupsEmpty(t *testing.T) {
	if contextGroups("") != nil || contextGroups("{}") != nil || contextGroups("not json") != nil {
		t.Fatal("пустой/битый контекст должен давать nil")
	}
}

// requestRows вытаскивает суть запроса из trace.data в фиксированном порядке.
func TestRequestRows(t *testing.T) {
	js := `{"trace":{"data":{"http.request.method":"GET","http.url":"https://x/y","route":"gotcha_test_boom"}}}`
	rows := requestRows(js)
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3: %v", len(rows), rows)
	}
	if rows[0].Key != "Method" || rows[0].Val != "GET" {
		t.Fatalf("row0=%v, want Method=GET", rows[0])
	}
	if rows[1].Key != "URL" || rows[2].Key != "Route" {
		t.Fatalf("порядок строк неверный: %v", rows)
	}
	if requestRows(`{"os":{"name":"x"}}`) != nil {
		t.Fatal("без trace.data → nil")
	}
}

func TestJSONScalar(t *testing.T) {
	cases := map[string]string{
		`"hi"`:        "hi",
		`42`:          "42",   // целое без .0
		`3.5`:         "3.5",
		`true`:        "true",
		`["a","b"]`:   `["a","b"]`,
		`null`:        "",
	}
	for in, want := range cases {
		if got := jsonScalar(json.RawMessage(in)); got != want {
			t.Errorf("jsonScalar(%s) = %q, want %q", in, got, want)
		}
	}
}

// parseBreadcrumbs понимает {"values":[…]} и прямой массив; label падает на type.
func TestParseBreadcrumbs(t *testing.T) {
	js := `{"values":[{"category":"query","level":"info","message":"SELECT 1"},{"type":"http","message":"GET /x","data":{"status":200}}]}`
	bc := parseBreadcrumbs(js)
	if len(bc) != 2 {
		t.Fatalf("bc=%d, want 2", len(bc))
	}
	if bc[0].Category != "query" || bc[0].Message != "SELECT 1" {
		t.Fatalf("bc0=%+v", bc[0])
	}
	if breadcrumbLabel(bc[1]) != "http" {
		t.Fatalf("label=%q, want http (нет category → type)", breadcrumbLabel(bc[1]))
	}
	if bc[1].Data == "" {
		t.Fatal("data breadcrumb должна сохраниться")
	}
	if len(parseBreadcrumbs(`[{"message":"a"}]`)) != 1 {
		t.Fatal("прямой массив должен парситься")
	}
	if parseBreadcrumbs("") != nil || parseBreadcrumbs("{}") != nil || parseBreadcrumbs("null") != nil {
		t.Fatal("пусто/null → nil")
	}
}

func TestBreadcrumbsViewRenders(t *testing.T) {
	var sb strings.Builder
	err := breadcrumbsView(`{"values":[{"category":"auth","level":"warning","message":"login failed"}]}`).Render(context.Background(), &sb)
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, w := range []string{"breadcrumbs", "auth", "login failed", "bc-level-warning"} {
		if !strings.Contains(out, w) {
			t.Fatalf("breadcrumbs не содержит %q: %s", w, out)
		}
	}
}

func TestContextTableRenders(t *testing.T) {
	var sb strings.Builder
	if err := contextTable([]ctxRow{{Key: "name", Val: "Linux"}}).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "ctx-table") || !strings.Contains(out, "Linux") || !strings.Contains(out, "name") {
		t.Fatalf("таблица контекста не отрендерилась: %s", out)
	}
}
