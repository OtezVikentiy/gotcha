package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestLangSwitcherAriaPressed: выбранный язык отличает только цвет — без
// aria-pressed скринридер не знает, какая кнопка активна (№84, тот же
// принцип, что у themePressed).
func TestLangSwitcherAriaPressed(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := langSwitcher().Render(ctx, &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `aria-pressed="true"`) || !strings.Contains(out, `aria-pressed="false"`) {
		t.Errorf("langSwitcher без пары aria-pressed true/false: %s", out)
	}
}
