package notify

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// Полнота словаря относительно реестра (kind.go) проверяется отдельно, в
// internal/guards — этот тест только про валидность самих i18n-ключей.
func TestRedactedKindLabelsResolveInBothCatalogs(t *testing.T) {
	if len(redactedKindKeys) < 15 {
		t.Fatalf("blind guard: redactedKindKeys has only %d entries — the map is broken", len(redactedKindKeys))
	}
	for kind, key := range redactedKindKeys {
		for _, code := range []string{"en", "ru"} {
			ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: code})
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("kind %q: ключ %q не найден в каталоге %s", kind, key, code)
			}
		}
	}
}
