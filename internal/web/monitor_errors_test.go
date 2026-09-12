package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestMonitorErrorsAreLocalized(t *testing.T) {
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"адрес", &uptime.ValidationError{Code: "http_url", Field: "url"}},
			{"регион", &uptime.ValidationError{Code: "region_unavailable", Field: "regions",
				Args: map[string]string{"region": "eu"}}},
			{"длина имени", &uptime.ValidationError{Code: "name_length", Field: "name",
				Args: map[string]string{"max": "200"}}},
		} {
			msg := monitorFormErrorMessage(ctx, tc.err)
			if strings.Contains(msg, "uptime:") || strings.Contains(msg, "invalid monitor") {
				t.Errorf("[%s/%s] сообщение несёт внутренности: %q", lang, tc.name, msg)
			}
			if strings.HasPrefix(msg, "error.monitor.") {
				t.Errorf("[%s/%s] ключ i18n не переведён: %q", lang, tc.name, msg)
			}
			if msg == "" {
				t.Errorf("[%s/%s] пустое сообщение", lang, tc.name)
			}
		}
	}
}

func TestValidationErrorStaysErrInvalidMonitor(t *testing.T) {
	err := error(&uptime.ValidationError{Code: "http_url"})
	if !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Error("ValidationError перестал быть ErrInvalidMonitor — сломаются все вызывающие")
	}
}
