package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

// renderFlame — рендер флеймграфа в строку; link по умолчанию — путь через «/».
func renderFlame(t *testing.T, root *profile.FlameNode, focus []string, width int) string {
	t.Helper()
	link := func(path []string) string {
		if path == nil {
			return "/flame"
		}
		return "/flame?focus=" + strings.Join(path, "&focus=")
	}
	var sb strings.Builder
	if err := flamegraphSVG(context.Background(), root, focus, width, link).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestFlamegraphSVG(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "aaaaaaaaaa", Value: 6},
		{Name: "bbbbbbbbbb", Value: 4},
	}}
	out := renderFlame(t, root, nil, 600)
	if !strings.Contains(out, "<svg") || !strings.Contains(out, "<rect") {
		t.Fatalf("svg missing rects: %s", out)
	}
	if !strings.Contains(out, "aaaaaaaaaa") || !strings.Contains(out, "bbbbbbbbbb") {
		t.Fatalf("svg missing frame names: %s", out)
	}
}

func TestFlamegraphSVGEmpty(t *testing.T) {
	out := renderFlame(t, &profile.FlameNode{Name: "all", Value: 0}, nil, 600)
	if !strings.Contains(out, "нет данных") {
		t.Fatalf("empty tree should render placeholder: %s", out)
	}
}

func TestFitFlameLabel(t *testing.T) {
	cases := []struct {
		name string
		w    float64
		want string
	}{
		// (70-4)/6.6 = 10 символов: «abcdefghij» влезает целиком.
		{"abcdefghij", 70, "abcdefghij"},
		// 11 символов в 10 не влезают: 9 рун + «…».
		{"abcdefghijk", 70, "abcdefghi…"},
		// Кириллица считается рунами, не байтами.
		{"абвгдежзийк", 70, "абвгдежзи…"},
		// fit=3 → на усечение остаётся 2 руны + «…» — подписи нет, только тултип.
		{"abcdefgh", 30, ""},
		// fit=4 — минимум, при котором усечение ещё имеет смысл.
		{"abcdefgh", 31, "abc…"},
		// Уже паддинга.
		{"ab", 3, ""},
		{"", 3, ""},
	}
	for _, c := range cases {
		if got := fitFlameLabel(c.name, c.w); got != c.want {
			t.Errorf("fitFlameLabel(%q, %v) = %q, want %q", c.name, c.w, got, c.want)
		}
	}
}

func TestFocusFlame(t *testing.T) {
	leaf := &profile.FlameNode{Name: "a", Value: 1}
	inner := &profile.FlameNode{Name: "a", Value: 3, Children: []*profile.FlameNode{leaf}}
	level1 := &profile.FlameNode{Name: "b", Value: 6, Children: []*profile.FlameNode{inner}}
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{level1}}

	node, anc, ok := focusFlame(root, nil)
	if node != root || anc != nil || !ok {
		t.Fatalf("empty path: node=%v anc=%v ok=%v", node, anc, ok)
	}
	node, anc, ok = focusFlame(root, []string{"b", "a"})
	if !ok || node != inner {
		t.Fatalf("path b/a: node=%v ok=%v, want inner", node, ok)
	}
	if !reflect.DeepEqual(anc, []*profile.FlameNode{root, level1}) {
		t.Fatalf("path b/a ancestors = %v, want [root level1]", anc)
	}
	// Одно имя на разных уровнях (a → a): спуск идёт по уровням, а не по первому совпадению.
	node, anc, ok = focusFlame(root, []string{"b", "a", "a"})
	if !ok || node != leaf || len(anc) != 3 || anc[2] != inner {
		t.Fatalf("path b/a/a: node=%v anc=%v ok=%v, want leaf with 3 ancestors", node, anc, ok)
	}
	// Оборванный путь — тихий откат к корню.
	node, anc, ok = focusFlame(root, []string{"b", "zzz"})
	if ok || node != root || anc != nil {
		t.Fatalf("broken path: node=%v anc=%v ok=%v, want root/nil/false", node, anc, ok)
	}
}

func TestFlamegraphSVGLinks(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "handler", Value: 6, Children: []*profile.FlameNode{{Name: "db", Value: 3}}},
		{Name: "gc", Value: 4},
	}}
	out := renderFlame(t, root, nil, 600)
	if n := strings.Count(out, "<a href="); n != 4 {
		t.Fatalf("want 4 links (one per node), got %d: %s", n, out)
	}
	if !strings.Contains(out, `<a href="/flame"><svg`) {
		t.Fatalf("root link must reset focus: %s", out)
	}
	if !strings.Contains(out, `<a href="/flame?focus=handler&amp;focus=db"><svg`) {
		t.Fatalf("db link must carry the full path: %s", out)
	}
	if strings.Contains(out, "<g>") {
		t.Fatalf("nodes must be nested <svg>, not <g>: %s", out)
	}
	if !strings.Contains(out, `<svg x="0.0" y="0" width="600.0" height="17">`) {
		t.Fatalf("root nested svg must span the full width: %s", out)
	}
	if !strings.Contains(out, `<rect x="0" y="0" width="100%" height="100%"`) {
		t.Fatalf("rect must fill its nested svg: %s", out)
	}
}

func TestFlamegraphSVGFocus(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "handler", Value: 6, Children: []*profile.FlameNode{
			{Name: "db", Value: 3},
			{Name: "render", Value: 3},
		}},
		{Name: "gc", Value: 4},
	}}
	out := renderFlame(t, root, []string{"handler"}, 600)
	// Предок «all» на всю ширину, полупрозрачный, со ссылкой-сбросом.
	if !strings.Contains(out, `<a href="/flame"><svg class="flame-ancestor" x="0.0" y="0" width="600.0" height="17">`) {
		t.Fatalf("root ancestor row missing: %s", out)
	}
	// Узел фокуса — на всю ширину во второй строке.
	if !strings.Contains(out, `<a href="/flame?focus=handler"><svg x="0.0" y="18" width="600.0" height="17">`) {
		t.Fatalf("focused node must span full width: %s", out)
	}
	// Дети масштабированы от node.Value: 600 × 3/6 = 300.
	if !strings.Contains(out, `<a href="/flame?focus=handler&amp;focus=db"><svg x="0.0" y="36" width="300.0" height="17">`) {
		t.Fatalf("db child must be scaled from focus: %s", out)
	}
	if !strings.Contains(out, `<svg x="300.0" y="36" width="300.0" height="17">`) {
		t.Fatalf("render child must follow db: %s", out)
	}
	// Тултип — доля от корня (3/10), не от фокуса (3/6).
	if !strings.Contains(out, "<title>db — 30.0%</title>") || strings.Contains(out, "50.0%") {
		t.Fatalf("tooltip share must be relative to root: %s", out)
	}
	// Соседи фокуса не рисуются.
	if strings.Contains(out, "gc") {
		t.Fatalf("sibling of focus must not be rendered: %s", out)
	}
	// Высота = (предки + глубина поддерева) × 18 = (1+2) × 18.
	if !strings.Contains(out, `viewBox="0 0 600 54"`) {
		t.Fatalf("height must cover ancestors and subtree: %s", out)
	}
}

func TestFlamegraphSVGFocusDeep(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "handler", Value: 6, Children: []*profile.FlameNode{
			{Name: "db", Value: 3, Children: []*profile.FlameNode{{Name: "query", Value: 1}}},
		}},
	}}
	out := renderFlame(t, root, []string{"handler", "db"}, 600)
	// Предок несёт СВОЙ путь, а не путь фокуса: клик по нему — шаг вверх.
	if !strings.Contains(out, `<a href="/flame?focus=handler"><svg class="flame-ancestor" x="0.0" y="18"`) {
		t.Fatalf("handler ancestor must link to its own path: %s", out)
	}
	if !strings.Contains(out, `<a href="/flame"><svg class="flame-ancestor" x="0.0" y="0"`) {
		t.Fatalf("root ancestor must link to the reset: %s", out)
	}
	if !strings.Contains(out, `<a href="/flame?focus=handler&amp;focus=db"><svg x="0.0" y="36" width="600.0"`) {
		t.Fatalf("focused db must span the full width on the third row: %s", out)
	}
	if !strings.Contains(out, `<a href="/flame?focus=handler&amp;focus=db&amp;focus=query"><svg x="0.0" y="54" width="200.0"`) {
		t.Fatalf("query child must be scaled from db (600 × 1/3): %s", out)
	}
}

func TestFlamegraphSVGBrokenFocusFallsBackToRoot(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{{Name: "handler", Value: 10}}}
	out := renderFlame(t, root, []string{"nope"}, 600)
	if strings.Contains(out, "flame-ancestor") || strings.Contains(out, "nope") {
		t.Fatalf("broken focus must render the root without ancestors: %s", out)
	}
	if !strings.Contains(out, `<a href="/flame?focus=handler">`) {
		t.Fatalf("child links must not inherit the broken path: %s", out)
	}
}

func TestFlamegraphSVGEscapesNames(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{{Name: "a&b<c>", Value: 10}}}
	out := renderFlame(t, root, nil, 600)
	if !strings.Contains(out, `href="/flame?focus=a&amp;b&lt;c&gt;"`) {
		t.Fatalf("href must be html-escaped: %s", out)
	}
	if !strings.Contains(out, "<title>a&amp;b&lt;c&gt; — 100.0%</title>") {
		t.Fatalf("title must be html-escaped: %s", out)
	}
	if !strings.Contains(out, `>a&amp;b&lt;c&gt;</text>`) {
		t.Fatalf("label must be html-escaped: %s", out)
	}
}

func TestFlamegraphSVGLabels(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10000, Children: []*profile.FlameNode{
		// 600 × 400/10000 = 24 — уже порога: rect есть, подписи нет.
		{Name: "tiny-frame", Value: 400},
		// 600 × 9599/10000 ≈ 576 → 86 символов; имя в 100 символов усекается с «…».
		{Name: strings.Repeat("x", 100), Value: 9599},
		// 600 × 1/10000 = 0.06 < 0.5 — кадр не рисуется вовсе, даже тултипа нет.
		{Name: "subpixel-frame", Value: 1},
	}}
	out := renderFlame(t, root, nil, 600)
	if strings.Contains(out, "subpixel-frame") {
		t.Fatalf("frame narrower than half a unit must be skipped entirely: %s", out)
	}
	if !strings.Contains(out, "<title>tiny-frame — 4.0%</title>") {
		t.Fatalf("narrow node must keep its tooltip: %s", out)
	}
	if strings.Contains(out, ">tiny-frame</text>") {
		t.Fatalf("narrow node must not get a label: %s", out)
	}
	if !strings.Contains(out, `">`+strings.Repeat("x", 85)+"…</text>") {
		t.Fatalf("long name must be truncated with an ellipsis: %s", out)
	}
	if !strings.Contains(out, `<text x="2" y="12"`) {
		t.Fatalf("label coordinates must be relative to the nested svg: %s", out)
	}
}

func TestFlameLink(t *testing.T) {
	r := httptest.NewRequest("GET", "/projects/7/profiles/flame?service=api&type=cpu&period=24h&focus=old", nil)
	link := flameLink(r)

	reset := link(nil)
	q, err := url.ParseQuery(strings.TrimPrefix(reset, "/projects/7/profiles/flame?"))
	if err != nil || strings.Contains(reset, "focus") || q.Get("service") != "api" || q.Get("period") != "24h" {
		t.Fatalf("reset link = %q (%v): must keep filters and drop focus", reset, err)
	}

	names := []string{"a+b c", "x&y=z", "юникод"}
	got := link(names)
	if !strings.HasPrefix(got, "/projects/7/profiles/flame?") {
		t.Fatalf("link = %q, want path prefix", got)
	}
	q, err = url.ParseQuery(strings.TrimPrefix(got, "/projects/7/profiles/flame?"))
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if !reflect.DeepEqual(q["focus"], names) {
		t.Fatalf("focus round-trip = %v, want %v", q["focus"], names)
	}
	if q.Get("type") != "cpu" || q.Get("service") != "api" {
		t.Fatalf("filters lost: %q", got)
	}
	// Исходный запрос билдер не портит.
	if r.URL.Query().Get("focus") != "old" {
		t.Fatalf("flameLink must not mutate the request query")
	}

	// Путь остаётся в экранированном виде: сырой «?» из trace_id начал бы query.
	esc := flameLink(httptest.NewRequest("GET", "/traces/a%3Fb/flame", nil))
	if got := esc([]string{"main"}); got != "/traces/a%3Fb/flame?focus=main" {
		t.Fatalf("escaped path link = %q", got)
	}

	// Без query — просто путь.
	bare := flameLink(httptest.NewRequest("GET", "/traces/t1/flame", nil))
	if got := bare(nil); got != "/traces/t1/flame" {
		t.Fatalf("bare reset link = %q", got)
	}
	if got := bare([]string{"main"}); got != "/traces/t1/flame?focus=main" {
		t.Fatalf("bare focus link = %q", got)
	}
}

// TestFlamegraphSVGZeroValueFocusNoNaN — K9-22: фокус на узле без сэмплов
// (Value == 0, ширину ему даёт зум) делил ширину детей на ноль: NaN/Inf
// проходил guard `w < 0.5` и уезжал в разметку как width="NaN". Теперь такой
// узел не рисуется вовсе: ни NaN, ни Inf, ни его кадра.
func TestFlamegraphSVGZeroValueFocusNoNaN(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "busy", Value: 10},
		{Name: "idle", Value: 0, Children: []*profile.FlameNode{{Name: "idlechild", Value: 0}}},
	}}
	out := renderFlame(t, root, []string{"idle"}, 600)
	for _, bad := range []string{"NaN", "Inf"} {
		if strings.Contains(out, bad) {
			t.Fatalf("zero-value focus leaked %q into markup: %s", bad, out)
		}
	}
	if strings.Contains(out, "idlechild") || strings.Contains(out, `<title>idle `) {
		t.Errorf("node without samples must not be drawn: %s", out)
	}
	// Предок (корень) при зуме остаётся полупрозрачной строкой.
	if !strings.Contains(out, "all") {
		t.Errorf("ancestor row missing: %s", out)
	}
}
