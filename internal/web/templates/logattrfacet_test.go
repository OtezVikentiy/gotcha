package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestLogAttrFacetSectionTooMuchData — деградация AttrKeys (таймаут/ошибка):
// секция печатает предупреждение и не пытается рисовать список ключей.
func TestLogAttrFacetSectionTooMuchData(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantNote := i18n.T(ctx, "logs.facet.too_much_data")
	var sb strings.Builder
	if err := logAttrFacetSection(1, LogsFilter{}, LogAttrFacets{TooMuchData: true}).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, wantNote) {
		t.Fatalf("TooMuchData должен показывать %q: %s", wantNote, out)
	}
	if strings.Contains(out, "logs-facet-list") {
		t.Fatalf("TooMuchData не должен рисовать список ключей: %s", out)
	}
}

// TestLogAttrFacetSectionEmpty — ключей нет (не TooMuchData): показывает
// отдельную пометку "пусто", а не список.
func TestLogAttrFacetSectionEmpty(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantNote := i18n.T(ctx, "logs.facet.empty")
	var sb strings.Builder
	if err := logAttrFacetSection(1, LogsFilter{}, LogAttrFacets{}).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, wantNote) {
		t.Fatalf("пустой список ключей должен показывать %q: %s", wantNote, out)
	}
	if strings.Contains(out, "logs-facet-list") {
		t.Fatalf("пустой список ключей не должен рисовать <ul class=logs-facet-list>: %s", out)
	}
}

// TestLogAttrFacetSectionCollapsedKey — ключ без Expanded: просто счётчик,
// без aria-current и без вложенного списка значений.
func TestLogAttrFacetSectionCollapsedKey(t *testing.T) {
	facet := LogAttrFacets{Keys: []LogAttrKeyFacet{
		{Key: "region", Count: 5, Href: "/p/1/logs?facet=region"},
	}}
	out := renderTo(t, logAttrFacetSection(1, LogsFilter{}, facet))
	if !strings.Contains(out, "region") || !strings.Contains(out, "5") {
		t.Fatalf("свёрнутый ключ должен показать имя и счётчик: %s", out)
	}
	if strings.Contains(out, "aria-current") {
		t.Fatalf("свёрнутый ключ не должен нести aria-current: %s", out)
	}
	if strings.Contains(out, "logs-facet-values") {
		t.Fatalf("свёрнутый ключ не должен рисовать вложенный список значений: %s", out)
	}
}

// TestLogAttrFacetSectionExpandedKeyNoValues — раскрытый ключ, но лениво
// подгруженные значения не пришли (отказ AttrValues, см. NewAttrFacets):
// ключ остаётся раскрытым (aria-current), но вместо списка — пометка пусто.
func TestLogAttrFacetSectionExpandedKeyNoValues(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantNote := i18n.T(ctx, "logs.facet.empty")
	facet := LogAttrFacets{Keys: []LogAttrKeyFacet{
		{Key: "host", Count: 3, Href: "/p/1/logs?facet=host", Expanded: true},
	}}
	var sb strings.Builder
	if err := logAttrFacetSection(1, LogsFilter{}, facet).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "aria-current") {
		t.Fatalf("раскрытый ключ должен нести aria-current: %s", out)
	}
	if !strings.Contains(out, wantNote) {
		t.Fatalf("раскрытый ключ без значений должен показать %q: %s", wantNote, out)
	}
	if strings.Contains(out, "logs-facet-values") {
		t.Fatalf("раскрытый ключ без значений не должен рисовать <ul class=logs-facet-values>: %s", out)
	}
}

// TestLogAttrFacetSectionExpandedKeyWithValues — раскрытый ключ со
// значениями: рендерит вложенный список, активное значение получает
// aria-current и модификатор класса -active, неактивное — нет.
func TestLogAttrFacetSectionExpandedKeyWithValues(t *testing.T) {
	facet := LogAttrFacets{Keys: []LogAttrKeyFacet{
		{Key: "host", Count: 3, Href: "/p/1/logs?facet=host", Expanded: true, Values: []LogAttrValueFacet{
			{Value: "web-1", Count: 2, Href: "/p/1/logs?attr=host:web-1", Active: true},
			{Value: "web-2", Count: 1, Href: "/p/1/logs?attr=host:web-2", Active: false},
		}},
	}}
	out := renderTo(t, logAttrFacetSection(1, LogsFilter{}, facet))
	if !strings.Contains(out, "logs-facet-values") {
		t.Fatalf("раскрытый ключ со значениями должен рисовать вложенный список: %s", out)
	}
	if !strings.Contains(out, "web-1") || !strings.Contains(out, "web-2") {
		t.Fatalf("должны быть оба значения: %s", out)
	}
	activeIdx := strings.Index(out, "web-1")
	inactiveIdx := strings.Index(out, "web-2")
	activeLi := out[:activeIdx]
	if !strings.Contains(out[max0(activeIdx-200, 0):activeIdx], "logs-facet-value-active") {
		t.Fatalf("активное значение web-1 должно нести класс -active: %s", out)
	}
	_ = activeLi
	if strings.Contains(out[max0(inactiveIdx-80, 0):inactiveIdx], "logs-facet-value-active") {
		t.Fatalf("неактивное значение web-2 не должно нести класс -active: %s", out)
	}
}

func max0(v, floor int) int {
	if v < floor {
		return floor
	}
	return v
}
