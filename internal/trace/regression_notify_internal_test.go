package trace

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestRegressionNotifyDurationIsNotInflated: значение duration приходит в
// миллисекундах, и 640 мс должны выглядеть как 640 мс. Прежняя копия
// форматирования в этом файле (formatMetric) трактовала их как миллисекунды
// уже переведённого duration, но не отличала duration от веб-виталов и не
// проверяла итог против humanize.MetricValue — здесь фиксируем контракт:
// 640/400 не должны превращаться в "640.0s"/"400.0s".
//
// Тест лежит в этом (внутреннем, package trace) файле, а не в
// regression_notify_test.go, потому что regressionSubject не экспортирован, а
// regression_notify_test.go — блэкбокс (package trace_test).
func TestRegressionNotifyDurationIsNotInflated(t *testing.T) {
	ctx := context.Background()
	ev := RegressionEvent{
		Kind: "regression_open", Target: "GET /api/items", Metric: "duration",
		BaselineValue: 400, CurrentValue: 640, PctIncrease: 0.6,
	}
	subj := regressionSubject(ctx, ev)
	if strings.Contains(subj, "640.0s") || strings.Contains(subj, "400.0s") {
		t.Fatalf("тема письма = %q: миллисекунды показаны как секунды", subj)
	}
	if !strings.Contains(subj, "640ms") || !strings.Contains(subj, "400ms") {
		t.Errorf("тема письма = %q, хотим значения в мс", subj)
	}
}

// TestRegressionNotifyLocale — subject/body регрессионных уведомлений строятся
// из каталога i18n по локали, положенной в контекст (№133–136): на ru — прежние
// русские тексты, на en — английские и без единого кириллического символа.
// Локаль в реальном коде подкладывает RegressionNotifier из GOTCHA_LOCALE.
func TestRegressionNotifyLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	open := RegressionEvent{
		Kind: "regression_open", Target: "GET /api/items", Metric: "duration",
		BaselineValue: 400, CurrentValue: 640, PctIncrease: 0.6,
	}
	closed := RegressionEvent{
		Kind: "regression_close", Target: "GET /api/items", Metric: "duration",
		BaselineValue: 640, CurrentValue: 400, DurationSeconds: 125,
	}
	const url = "https://gotcha.example/projects/1/regressions"

	containsCyrillic := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) })
	}

	for _, tc := range []struct {
		name string
		got  string
		want string // подстрока
	}{
		{"ru subject open", regressionSubject(ru, open), "Регрессия"},
		{"ru subject close", regressionSubject(ru, closed), "Регрессия устранена"},
		{"ru body open", regressionBody(ru, open, url), "Обнаружена регрессия"},
		{"ru body close", regressionBody(ru, closed, url), "Длительность: 2m5s"},
		{"en subject open", regressionSubject(en, open), "Regression"},
		{"en subject close", regressionSubject(en, closed), "Regression resolved"},
		{"en body open", regressionBody(en, open, url), "Regression detected"},
		{"en body close", regressionBody(en, closed, url), "Duration: 2m5s"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s = %q, хотим подстроку %q", tc.name, tc.got, tc.want)
		}
		if strings.HasPrefix(tc.name, "en") && containsCyrillic(tc.got) {
			t.Errorf("%s = %q: кириллица на en-локали", tc.name, tc.got)
		}
	}
	// Подстановки различимы: проценты и значения в открытии.
	if s := regressionSubject(en, open); !strings.Contains(s, "+60%") {
		t.Errorf("en subject open = %q, хотим +60%%", s)
	}
}
