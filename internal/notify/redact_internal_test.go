package notify

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestRedactedKindLabelsCoverAllKinds — сторож словаря redactedKindKeys:
// каждый заявленный kind обязан резолвиться в настоящий ключ ОБОИХ каталогов
// (иначе получатель снова увидит сырой enum — ровно то, что чинил QA MINOR-4),
// и сам словарь обязан покрывать все виды, которые шлют нотифаеры.
func TestRedactedKindLabelsCoverAllKinds(t *testing.T) {
	// Полный перечень видов по нотифаерам (см. комментарии в redactedKindKeys).
	// Новый вид алерта обязан попасть и сюда, и в словарь — тест на пару с
	// код-ревью держит их синхронными.
	allKinds := []string{
		"new_issue", "regression", "spike",
		"suppressed_digest",
		"metric_alert_open", "metric_alert_resolved",
		"n_plus_one", "slow_db_query", "http_flood",
		"regression_open", "regression_close",
		"slo_burn_open", "slo_burn_close",
		"profile_regression_open", "profile_regression_resolved",
		"down", "up", "ssl_expiring", "reminder",
		"host_alert_open", "host_alert_resolved", "host_retired",
	}
	for _, kind := range allKinds {
		key, ok := redactedKindKeys[kind]
		if !ok {
			t.Errorf("kind %q отсутствует в redactedKindKeys", kind)
			continue
		}
		for _, code := range []string{"en", "ru"} {
			ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: code})
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("kind %q: ключ %q не найден в каталоге %s", kind, key, code)
			}
		}
	}
	for kind := range redactedKindKeys {
		found := false
		for _, k := range allKinds {
			if k == kind {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("redactedKindKeys несёт лишний kind %q — добавь его в allKinds или удали", kind)
		}
	}
}
