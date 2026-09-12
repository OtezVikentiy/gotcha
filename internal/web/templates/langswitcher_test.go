package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

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
