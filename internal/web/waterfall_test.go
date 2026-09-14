package web

import (
	"context"
	"html"
	"strings"
	"testing"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestFitWaterfallLabel(t *testing.T) {
	cases := []struct {
		label string
		avail float64
		want  string
	}{
		// (300-4-4)/6.6 ≈ 44 руны — эталонная длина при полной глубине labelX=0.
		{"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqr", 292, "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqr"},
		{strings.Repeat("x", 60), 292, strings.Repeat("x", 43) + "…"},
		{"abcdefgh", 26, ""},
		// кириллица и спецсимволы — по 2 и 3 байта на руну; резка байтами
		// вместо рун обрежет ровно посреди символа.
		{strings.Repeat("х", 60), 292, strings.Repeat("х", 43) + "…"},
		{strings.Repeat("→", 60), 292, strings.Repeat("→", 43) + "…"},
	}
	for _, c := range cases {
		got := fitWaterfallLabel(c.label, c.avail)
		if got != c.want {
			t.Errorf("fitWaterfallLabel(%q, %v) = %q, want %q", c.label, c.avail, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("fitWaterfallLabel(%q, %v) = %q — невалидный UTF-8, резка прошла посреди руны", c.label, c.avail, got)
		}
	}
}

func TestWaterfallMarkupTruncatesLongLabel(t *testing.T) {
	longSQL := "SELECT " + strings.Repeat("very_long_column_name, ", 20) + "id FROM orders"
	spans := []trace.SpanRow{{
		SpanID:      "root",
		Description: longSQL,
		DurationUS:  1500,
	}}
	var sb strings.Builder
	if err := waterfallSVG(context.Background(), spans, nil, 100000, waterfallWidth).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()

	full := html.EscapeString(waterfallLabel(spans[0]))
	if !strings.Contains(out, "<title>"+full+"</title>") {
		t.Fatalf("полный текст должен остаться в <title>: %s", out)
	}
	if strings.Contains(out, `class="waterfall-label">`+full+`</text>`) {
		t.Fatalf("видимая подпись не должна содержать полный (необрезанный) текст: %s", out)
	}
	if !strings.Contains(out, "…</text>") {
		t.Fatalf("видимая подпись должна быть усечена многоточием: %s", out)
	}
}

func TestWaterfallMarkupShortLabelKeepsFullText(t *testing.T) {
	spans := []trace.SpanRow{{
		SpanID:     "root",
		Op:         "db.query",
		DurationUS: 500,
	}}
	var sb strings.Builder
	if err := waterfallSVG(context.Background(), spans, nil, 1000, waterfallWidth).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	full := html.EscapeString(waterfallLabel(spans[0]))
	if !strings.Contains(out, ">"+full+"<") {
		t.Fatalf("короткая подпись должна остаться нетронутой: %s", out)
	}
	if strings.Contains(out, "…") {
		t.Fatalf("короткая подпись не должна усекаться: %s", out)
	}
}

// span_id/parent_span_id крафтятся клиентом (Sentry-совместимый приём) —
// кольцо без единого настоящего корня не должно опустошать вывод.
func TestOrderSpanTreeBreaksRootlessCycle(t *testing.T) {
	spans := []trace.SpanRow{
		{SpanID: "a", ParentSpanID: "b", Op: "a"},
		{SpanID: "b", ParentSpanID: "a", Op: "b"},
	}
	ordered := orderSpanTree(spans, waterfallMaxRows)
	if len(ordered) != len(spans) {
		t.Fatalf("orderSpanTree(кольцо a<->b) вернул %d узлов, ожидалось %d — кольцо без root не должно опустошать вывод", len(ordered), len(spans))
	}
	seen := map[string]bool{}
	for _, o := range ordered {
		seen[o.span.SpanID] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("orderSpanTree(кольцо a<->b) = %+v, оба узла обязаны попасть в вывод", ordered)
	}
}

func TestWaterfallMarkupRootlessCycleNotEmpty(t *testing.T) {
	spans := []trace.SpanRow{
		{SpanID: "a", ParentSpanID: "b", Op: "a", DurationUS: 100},
		{SpanID: "b", ParentSpanID: "a", Op: "b", DurationUS: 100},
	}
	out := waterfallMarkup(context.Background(), spans, nil, 1000, waterfallWidth)
	if out == "" {
		t.Fatal("waterfallMarkup на кольце span_id/parent_span_id вернул пустую строку — страница трейса покажет пустое место")
	}
	if !strings.Contains(out, "<rect") {
		t.Errorf("waterfallMarkup на кольце обязан нарисовать хотя бы один <rect>: %s", out)
	}
}
