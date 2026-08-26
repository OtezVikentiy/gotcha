package uptime

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestUptimeNotifyLocale — subject/body uptime-уведомлений строятся из
// каталога i18n по локали в контексте (класс №133–136): на en — прежние
// английские тексты, на ru — русские. Локаль в реальном коде подкладывает
// OutboxNotifier из GOTCHA_LOCALE; до этой правки тексты были зашиты
// по-английски и ru-инстанс слал алерты на чужом языке.
func TestUptimeNotifyLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	const url = "https://gotcha.example/monitors/7"

	mon := Monitor{ID: 7, Name: "api-prod"}
	base := Event{Monitor: mon, Regions: []string{"local"}, Cause: "timeout"}
	down, up, ssl, reminder, generic := base, base, base, base, base
	down.Kind = "down"
	up.Kind, up.DurationSeconds = "up", 125
	ssl.Kind, ssl.DaysLeft = "ssl_expiring", 14
	reminder.Kind, reminder.DurationSeconds = "reminder", 3700
	generic.Kind = "flapping"

	containsCyrillic := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) })
	}

	for _, tc := range []struct {
		name string
		got  string
		want string // подстрока
	}{
		{"en subject down", subjectFor(en, down), "api-prod is DOWN"},
		{"en subject up", subjectFor(en, up), "back UP (2m5s)"},
		{"en subject ssl", subjectFor(en, ssl), "expires in 14 days"},
		{"en subject reminder", subjectFor(en, reminder), "still DOWN (1h1m)"},
		{"en subject generic", subjectFor(en, generic), "api-prod: flapping"},
		{"en body down", bodyFor(en, down, url, ""), "Cause: timeout"},
		{"en body up", bodyFor(en, up, url, ""), "back UP after 2m5s"},
		{"en body ssl", bodyFor(en, ssl, url, ""), "expires in 14 days"},
		{"en body reminder", bodyFor(en, reminder, url, ""), "1h1m so far"},
		{"en body generic", bodyFor(en, generic, url, ""), url},
		{"ru subject down", subjectFor(ru, down), "api-prod недоступен"},
		{"ru subject up", subjectFor(ru, up), "снова доступен (2m5s)"},
		{"ru subject ssl", subjectFor(ru, ssl), "истекает через 14 дн"},
		{"ru subject reminder", subjectFor(ru, reminder), "всё ещё недоступен (1h1m)"},
		{"ru body down", bodyFor(ru, down, url, ""), "Причина: timeout"},
		{"ru body reminder", bodyFor(ru, reminder, url, ""), "Регионы: local"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s = %q, хотим подстроку %q", tc.name, tc.got, tc.want)
		}
		if strings.HasPrefix(tc.name, "en") && containsCyrillic(tc.got) {
			t.Errorf("%s = %q: кириллица на en-локали", tc.name, tc.got)
		}
	}
}
