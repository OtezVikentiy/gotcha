package web_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// TestHSTSHeaderValue — сборка значения Strict-Transport-Security из четырёх
// настроек. Функция ТОТАЛЬНА и ничего не валидирует: отрицательный max-age
// (валидация которого — дело конфига и отказ старта) даёт пустую строку, а не
// битый заголовок, — у функции есть и другие вызывающие, кроме main.go.
func TestHSTSHeaderValue(t *testing.T) {
	for _, tc := range []struct {
		name              string
		enabled           bool
		maxAge            int
		includeSubDomains bool
		preload           bool
		want              string
	}{
		{"disabled", false, 31536000, true, true, ""},
		{"default", true, 31536000, false, false, "max-age=31536000"},
		{"zero unpins", true, 0, false, false, "max-age=0"},
		{"subdomains", true, 31536000, true, false, "max-age=31536000; includeSubDomains"},
		{"preload", true, 31536000, true, true, "max-age=31536000; includeSubDomains; preload"},
		{"preload without subdomains is built as asked", true, 31536000, false, true, "max-age=31536000; preload"},
		{"custom max-age", true, 600, false, false, "max-age=600"},
		{"negative max-age yields nothing", true, -1, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := web.HSTSHeaderValue(tc.enabled, tc.maxAge, tc.includeSubDomains, tc.preload)
			if got != tc.want {
				t.Errorf("HSTSHeaderValue(%v, %d, %v, %v) = %q, want %q",
					tc.enabled, tc.maxAge, tc.includeSubDomains, tc.preload, got, tc.want)
			}
		})
	}
}
