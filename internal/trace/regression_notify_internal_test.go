package trace

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// здесь (package trace), не в regression_notify_test.go — regressionSubject
// не экспортирован, тот файл блэкбокс (package trace_test).
func TestRegressionNotifyDurationIsNotInflated(t *testing.T) {
	ctx := context.Background()
	ev := RegressionEvent{
		Kind: notify.KindRegressionOpen, Target: "GET /api/items", Metric: "duration",
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

func TestRegressionNotifyLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	open := RegressionEvent{
		Kind: notify.KindRegressionOpen, Target: "GET /api/items", Metric: "duration",
		BaselineValue: 400, CurrentValue: 640, PctIncrease: 0.6,
	}
	closed := RegressionEvent{
		Kind: notify.KindRegressionClose, Target: "GET /api/items", Metric: "duration",
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
	if s := regressionSubject(en, open); !strings.Contains(s, "+60%") {
		t.Errorf("en subject open = %q, хотим +60%%", s)
	}
}
