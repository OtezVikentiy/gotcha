package metric

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestMetricNotifyLocale — subject/body метрик-алертов строятся из каталога
// i18n по локали в контексте (класс №133–136). До правки тексты были зашиты
// по-английски.
func TestMetricNotifyLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	const url = "https://gotcha.example/projects/1/metrics/alerts"
	ev := MetricEvent{
		MetricName: "http_errors", Aggregation: "sum", Comparator: "gt",
		Threshold: 100, Current: 150, Peak: 200, Opened: true,
	}
	closed := ev
	closed.Opened = false

	if s := metricSubject(en, ev); !strings.Contains(s, "Metric http_errors sum > 100 (firing)") {
		t.Errorf("en subject = %q", s)
	}
	if s := metricSubject(ru, closed); !strings.Contains(s, "Метрика http_errors sum > 100 (разрешилось)") {
		t.Errorf("ru subject = %q", s)
	}
	if b := metricBody(en, ev, url); !strings.Contains(b, "Metric threshold breached.") ||
		!strings.Contains(b, "Scope: all environments") {
		t.Errorf("en body = %q", b)
	}
	if b := metricBody(ru, closed, url); !strings.Contains(b, "Порог метрики вернулся в норму.") ||
		!strings.Contains(b, "Охват: все окружения") {
		t.Errorf("ru body = %q", b)
	}
	for _, s := range []string{metricSubject(en, ev), metricBody(en, ev, url)} {
		if strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
			t.Errorf("кириллица на en-локали: %q", s)
		}
	}
}
