package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

// renderFlame — рендер флеймграфа в строку.
func renderFlame(t *testing.T, root *profile.FlameNode, width int) string {
	t.Helper()
	var sb strings.Builder
	if err := flamegraphSVG(context.Background(), root, width).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestFlamegraphSVG(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "aaaaaaaaaa", Value: 6},
		{Name: "bbbbbbbbbb", Value: 4},
	}}
	out := renderFlame(t, root, 600)
	if !strings.Contains(out, "<svg") || !strings.Contains(out, "<rect") {
		t.Fatalf("svg missing rects: %s", out)
	}
	if !strings.Contains(out, "aaaaaaaaaa") || !strings.Contains(out, "bbbbbbbbbb") {
		t.Fatalf("svg missing frame names: %s", out)
	}
}

func TestFlamegraphSVGEmpty(t *testing.T) {
	out := renderFlame(t, &profile.FlameNode{Name: "all", Value: 0}, 600)
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

func TestFlamegraphSVGNestedFrames(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{
		{Name: "handler", Value: 6, Children: []*profile.FlameNode{{Name: "db", Value: 3}}},
		{Name: "gc", Value: 4},
	}}
	out := renderFlame(t, root, 600)
	if strings.Contains(out, "<g>") {
		t.Fatalf("nodes must be nested <svg>, not <g>: %s", out)
	}
	if !strings.Contains(out, `<svg x="0.0" y="0" width="600.0" height="17">`) {
		t.Fatalf("root nested svg must span the full width: %s", out)
	}
	if !strings.Contains(out, `<svg x="0.0" y="36" width="180.0" height="17">`) {
		t.Fatalf("db frame must be 600 × 3/10 wide on the third row: %s", out)
	}
	if !strings.Contains(out, `<rect x="0" y="0" width="100%" height="100%"`) {
		t.Fatalf("rect must fill its nested svg: %s", out)
	}
	if strings.Contains(out, `font-size="10"`) {
		t.Fatalf("font size must come from the stylesheet, not an attribute: %s", out)
	}
}
func TestFlamegraphSVGEscapesNames(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 10, Children: []*profile.FlameNode{{Name: "a&b<c>", Value: 10}}}
	out := renderFlame(t, root, 600)
	if !strings.Contains(out, "<title>a&amp;b&lt;c&gt; — 100.0%</title>") {
		t.Fatalf("title must be html-escaped: %s", out)
	}
	if !strings.Contains(out, `>a&amp;b&lt;c&gt;</text>`) {
		t.Fatalf("label must be html-escaped: %s", out)
	}
}

func TestFlamegraphSVGLabels(t *testing.T) {
	root := &profile.FlameNode{Name: "all", Value: 100, Children: []*profile.FlameNode{
		// 600 × 4/100 = 24 — уже порога: rect есть, подписи нет.
		{Name: "tiny-frame", Value: 4},
		// 600 × 96/100 = 576 → 86 символов; имя в 100 символов усекается с «…».
		{Name: strings.Repeat("x", 100), Value: 96},
	}}
	out := renderFlame(t, root, 600)
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
