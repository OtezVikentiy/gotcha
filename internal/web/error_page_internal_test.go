package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

func TestRenderErrorLeaksNothing(t *testing.T) {
	h := &Handler{}
	for _, status := range []int{404, 500} {
		rec := httptest.NewRecorder()
		h.renderError(rec, httptest.NewRequest("GET", "/whatever", nil), status, "boom")
		body := rec.Body.String()

		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
		if v := version.Version(); v != "" && strings.Contains(body, v) {
			t.Errorf("страница ошибки %d содержит версию сборки %q", status, v)
		}
		for _, marker := range []string{"goroutine ", ".go:", "runtime.", "panic:"} {
			if strings.Contains(body, marker) {
				t.Errorf("страница ошибки %d содержит след стека %q", status, marker)
			}
		}
	}
}
