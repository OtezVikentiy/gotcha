package web

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func renderDepsMap(t *testing.T, deps []templates.DependencyRow) string {
	t.Helper()
	var sb strings.Builder
	if err := dependencyMapSVG(context.Background(), deps, depsMapWidth).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func depsMapHeight(t *testing.T, out string) int {
	t.Helper()
	m := regexp.MustCompile(`viewBox="0 0 760 (\d+)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("нет viewBox шириной 760: %s", out)
	}
	h, _ := strconv.Atoi(m[1])
	return h
}

func depsNodeYs(out string, x int) []int {
	re := regexp.MustCompile(`<rect x="` + strconv.Itoa(x) + `" y="(\d+)"`)
	var ys []int
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		y, _ := strconv.Atoi(m[1])
		ys = append(ys, y)
	}
	return ys
}

func TestDependencyMapSVG(t *testing.T) {
	deps := []templates.DependencyRow{
		{Kind: "database", Target: "postgresql", Calls: 1200, P50US: 3000, P95US: 8000, ErrorRate: 0.001, Direction: "both"},
		{Kind: "http", Target: "api.stripe.com", Calls: 40, P50US: 60000, P95US: 120000, ErrorRate: 0.02, Direction: "out"},
	}
	out := renderDepsMap(t, deps)
	for _, want := range []string{
		`<svg class="deps-map`, "postgresql", "api.stripe.com",
		`1.2k · p95 8ms · 0.1%`, `40 · p95 120ms · 2.0%`,
		`<title>postgresql: 1200 · p50 3ms · p95 8ms · 0.1%</title>`,
		`deps-node deps-center`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SVG не содержит %q: %s", want, out)
		}
	}
	if got := strings.Count(out, `<path class="deps-edge`); got != 2 {
		t.Errorf("рёбер = %d, ожидалось 2: %s", got, out)
	}
	if !regexp.MustCompile(`<path class="deps-edge[^"]*" d="M [\d.]+ [\d.]+ C `).MatchString(out) {
		t.Errorf("ребро не кривая Безье от хаба: %s", out)
	}
	if !strings.Contains(out, `class="deps-edge deps-edge-warn"`) {
		t.Errorf("ребро с 2%% ошибок не помечено warn: %s", out)
	}
	if out != renderDepsMap(t, deps) {
		t.Errorf("рендер недетерминирован: два прогона разошлись")
	}
}

func TestDependencyMapSVGSides(t *testing.T) {
	mixed := []templates.DependencyRow{
		{Kind: "database", Target: "postgresql"},
		{Kind: "http", Target: "api.stripe.com"},
		{Kind: "cache", Target: "redis"},
	}
	out := renderDepsMap(t, mixed)
	if l, r := depsNodeYs(out, 24), depsNodeYs(out, 516); len(l) != 2 || len(r) != 1 {
		t.Errorf("смешанный набор: слева %d, справа %d, ожидалось 2/1: %s", len(l), len(r), out)
	}
	if pi, ri := strings.Index(out, "postgresql"), strings.Index(out, "redis"); pi > ri {
		t.Errorf("порядок узлов слева нарушен: postgresql должен идти первым")
	}

	onlyHTTP := []templates.DependencyRow{
		{Kind: "http", Target: "a.example"}, {Kind: "http", Target: "b.example"},
	}
	out = renderDepsMap(t, onlyHTTP)
	if l, r := depsNodeYs(out, 24), depsNodeYs(out, 516); len(l) != 0 || len(r) != 2 {
		t.Errorf("только http: слева %d, справа %d, ожидалось 0/2: %s", len(l), len(r), out)
	}

	onlyStore := []templates.DependencyRow{
		{Kind: "database", Target: "postgresql"}, {Kind: "cache", Target: "redis"},
	}
	out = renderDepsMap(t, onlyStore)
	if l, r := depsNodeYs(out, 24), depsNodeYs(out, 516); len(l) != 2 || len(r) != 0 {
		t.Errorf("только хранилища: слева %d, справа %d, ожидалось 2/0: %s", len(l), len(r), out)
	}
}

func TestDependencyMapSVGNoOverlap(t *testing.T) {
	var deps []templates.DependencyRow
	for i := 0; i < 12; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "database", Target: fmt.Sprintf("db-%02d", i)})
	}
	for i := 0; i < 4; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%02d", i)})
	}
	out := renderDepsMap(t, deps)
	for _, x := range []int{24, 516} {
		ys := depsNodeYs(out, x)
		for i := range ys {
			for j := i + 1; j < len(ys); j++ {
				if d := ys[j] - ys[i]; d < 60 && d > -60 {
					t.Errorf("узлы колонки x=%d накладываются: y=%d и y=%d", x, ys[i], ys[j])
				}
			}
		}
	}
	if l := depsNodeYs(out, 24); len(l) != 12 {
		t.Fatalf("слева %d узлов, ожидалось 12", len(l))
	}
	if got := depsMapHeight(t, out); got != 12*60+32 {
		t.Errorf("высота = %d, ожидалось %d (12 строк слева)", got, 12*60+32)
	}
	ys := depsNodeYs(out, 24)
	if last := ys[len(ys)-1] + 44; last > 12*60+32 {
		t.Errorf("последний узел (низ %d) ниже края карты", last)
	}
}

func TestDependencyMapSVGHeight(t *testing.T) {
	one := []templates.DependencyRow{{Kind: "http", Target: "a.example"}}
	out := renderDepsMap(t, one)
	if got := depsMapHeight(t, out); got != 120 {
		t.Errorf("1 узел: высота = %d, ожидалось 120", got)
	}
	if ys := depsNodeYs(out, 516); len(ys) != 1 || ys[0] != 38 {
		t.Errorf("1 узел: y = %v, ожидалось [38]", ys)
	}

	var deps []templates.DependencyRow
	for i := 0; i < 5; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "database", Target: fmt.Sprintf("db-%d", i)})
	}
	for i := 0; i < 2; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%d", i)})
	}
	out = renderDepsMap(t, deps)
	if got := depsMapHeight(t, out); got != 332 {
		t.Errorf("5/2: высота = %d, ожидалось 332", got)
	}
	if ys := depsNodeYs(out, 516); len(ys) != 2 || ys[0] != 114 || ys[1] != 174 {
		t.Errorf("5/2: правая колонка y = %v, ожидалось [114 174]", ys)
	}

	var many []templates.DependencyRow
	for i := 0; i < depsMapNodeCap+1; i++ {
		many = append(many, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%02d", i)})
	}
	out = renderDepsMap(t, many)
	if got := depsMapHeight(t, out); got != depsMapNodeCap*60+32+24 {
		t.Errorf("с хвостом: высота = %d, ожидалось %d", got, depsMapNodeCap*60+32+24)
	}
	if !strings.Contains(out, "+1 ") {
		t.Errorf("с хвостом: нет пометки «+1 ещё»: %s", out)
	}
}

// координаты выведены из формул раскладки (5 БД/2 http, H=332, cy=166), не
// скопированы из вывода — держатся на порядке хаб→узел (marker-start у хаба).
func TestDependencyMapSVGGeometry(t *testing.T) {
	var deps []templates.DependencyRow
	for i := 0; i < 5; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "database", Target: fmt.Sprintf("db-%d", i)})
	}
	for i := 0; i < 2; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%d", i)})
	}
	out := renderDepsMap(t, deps)
	for _, want := range []string{
		`<path class="deps-edge" d="M 450 154.0 C 483.0 154.0 483.0 136.0 516 136.0"`,
		`<path class="deps-edge" d="M 310 146.8 C 277.0 146.8 277.0 46.0 244 46.0"`,
		`<path class="deps-edge" d="M 450 178.0 C 483.0 178.0 483.0 196.0 516 196.0"`,
		`<g class="deps-node deps-center"><rect x="310" y="142" rx="6" width="140" height="48"/>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет фрагмента %q: %s", want, out)
		}
	}
}

func TestDependencyMapSVGMarkers(t *testing.T) {
	cases := []struct {
		dir        string
		start, end bool
	}{
		{"in", true, false},
		{"out", false, true},
		{"both", true, true},
		{"none", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		out := renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "x.example", Direction: c.dir}})
		if got := strings.Contains(out, `marker-start="url(#deps-arrow-ok)"`); got != c.start {
			t.Errorf("dir=%q: marker-start = %v, ожидалось %v: %s", c.dir, got, c.start, out)
		}
		if got := strings.Contains(out, `marker-end="url(#deps-arrow-ok)"`); got != c.end {
			t.Errorf("dir=%q: marker-end = %v, ожидалось %v: %s", c.dir, got, c.end, out)
		}
	}
	out := renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "x.example", Direction: "both"}})
	const attrs = `viewBox="0 0 10 8" refX="10" refY="4" markerWidth="10" markerHeight="8" markerUnits="userSpaceOnUse" orient="auto-start-reverse"`
	for _, id := range []string{"deps-arrow-ok", "deps-arrow-warn", "deps-arrow-bad"} {
		if !strings.Contains(out, `<marker id="`+id+`" class="`+id+`" `+attrs+`>`) {
			t.Errorf("нет определения маркера %s с полным набором атрибутов: %s", id, out)
		}
	}
	out = renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "x.example", Direction: "out", ErrorRate: 0.1}})
	if !strings.Contains(out, `marker-end="url(#deps-arrow-bad)"`) {
		t.Errorf("10%% ошибок: маркер не bad: %s", out)
	}
	out = renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "x.example", Direction: "in", ErrorRate: 0.01}})
	if !strings.Contains(out, `marker-start="url(#deps-arrow-warn)"`) {
		t.Errorf("1%% ошибок: маркер не warn: %s", out)
	}
}

func TestDependencyMapSVGTruncate(t *testing.T) {
	long := strings.Repeat("a", 40)
	out := renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: long}})
	if !strings.Contains(out, "…</text>") {
		t.Errorf("длинное имя не усечено многоточием: %s", out)
	}
	if strings.Contains(out, ">"+long+"</text>") {
		t.Errorf("длинное имя выведено целиком в подпись: %s", out)
	}
	if !strings.Contains(out, "<title>"+long+"</title>") {
		t.Errorf("нет <title> с полным именем: %s", out)
	}

	out = renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "short.example"}})
	if strings.Contains(out, "<title>short.example</title>") {
		t.Errorf("короткое имя получило лишний <title>: %s", out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("короткое имя усечено: %s", out)
	}
	out = renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: "<b>x</b>"}})
	if strings.Contains(out, "<b>") || !strings.Contains(out, "&lt;b&gt;") {
		t.Errorf("имя не экранировано: %s", out)
	}
	longTag := strings.Repeat("a", 30) + "<b>"
	out = renderDepsMap(t, []templates.DependencyRow{{Kind: "http", Target: longTag}})
	if !strings.Contains(out, "<title>"+strings.Repeat("a", 30)+"&lt;b&gt;</title>") || strings.Contains(out, "<b>") {
		t.Errorf("полное имя в <title> не экранировано: %s", out)
	}
}

func TestDepCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1.0k"}, {1200, "1.2k"}, {12345, "12.3k"},
		{999_999, "1.0M"}, {1_500_000, "1.5M"},
	}
	for _, c := range cases {
		if got := depCount(c.n); got != c.want {
			t.Errorf("depCount(%d) = %q, ожидалось %q", c.n, got, c.want)
		}
	}
}

func TestDependencyMapSVGCap(t *testing.T) {
	if depsMapNodeCap != 16 {
		t.Fatalf("depsMapNodeCap = %d, ожидалось 16", depsMapNodeCap)
	}
	var deps []templates.DependencyRow
	for i := 0; i < depsMapNodeCap+4; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%02d", i), Calls: int64(100 - i)})
	}
	out := renderDepsMap(t, deps)
	if !strings.Contains(out, fmt.Sprintf("svc-%02d", depsMapNodeCap-1)) {
		t.Errorf("узел в пределах кэпа не нарисован: %s", out)
	}
	if strings.Contains(out, fmt.Sprintf("svc-%02d", depsMapNodeCap)) {
		t.Errorf("узел за кэпом (svc-%02d) не должен рисоваться на карте", depsMapNodeCap)
	}
	// класс `deps-node deps-center` у центра без кавычки после — не совпадёт.
	if got := strings.Count(out, `deps-node"`); got != depsMapNodeCap {
		t.Errorf("узлов-зависимостей на карте = %d, ожидалось %d (кэп)", got, depsMapNodeCap)
	}
	if !strings.Contains(out, "deps-more") || !strings.Contains(out, "+4 ") {
		t.Errorf("нет пометки «+4 ещё» при превышении кэпа: %s", out)
	}
}
