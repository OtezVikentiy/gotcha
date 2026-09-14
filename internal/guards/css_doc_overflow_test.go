package guards

import (
	"strings"
	"testing"
)

// /docs/* цитирует переменные окружения прямо в прозе без backticks — без
// overflow-wrap: anywhere длинный токен распирает страницу на узком экране.
func TestDocContentTextWrapsArbitraryValues(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	for _, selector := range []string{".doc-content p", ".doc-content li"} {
		found := false
		wraps := false
		for _, b := range blocks {
			if b.AtRule != "" {
				continue
			}
			for _, part := range strings.Split(b.Selector, ",") {
				if strings.TrimSpace(part) != selector {
					continue
				}
				found = true
				if v, ok := declValue(b.Body, "overflow-wrap"); ok && strings.TrimSpace(v) == "anywhere" {
					wraps = true
				}
			}
		}
		if !found {
			t.Fatalf("селектор %q не найден в app.css", selector)
		}
		if !wraps {
			t.Errorf("%s: ни один блок с этим селектором не задаёт overflow-wrap: anywhere — без него длинное значение без пробелов распирает страницу", selector)
		}
	}
}

// "Производительность" — единственный из десяти ярлыков рейла шире 87px;
// перенос на вторую строку решает это без изменения ширины рейла.
func TestRailLabelWrapsInsteadOfTruncating(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)
	b := selectorBlock(t, blocks, ".rail-label")

	if v, _ := declValue(b.Body, "white-space"); v == "nowrap" {
		t.Errorf(".rail-label: white-space: nowrap форсирует ellipsis-обрезку длинных ярлыков вместо переноса на вторую строку")
	}
	if v, ok := declValue(b.Body, "overflow-wrap"); !ok || strings.TrimSpace(v) != "anywhere" {
		t.Errorf(".rail-label: overflow-wrap = %q, want \"anywhere\" — без него перенос невозможен внутри одного длинного слова", v)
	}
}

// .help-label цитирует сырое имя настройки в скобках без пробелов — без переноса
// это распирает узкую колонку формы (project-perf-form, minmax(150px, 1fr)).
func TestHelpLabelWrapsSettingKeyToken(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)
	b := selectorBlock(t, blocks, ".help-label")

	if v, ok := declValue(b.Body, "overflow-wrap"); !ok || strings.TrimSpace(v) != "anywhere" {
		t.Errorf(".help-label: overflow-wrap = %q, want \"anywhere\" — сырое имя настройки в скобках распирает узкую колонку формы", v)
	}
}
