package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RawQuery выставлен напрямую в обход httptest.NewRequest: url.ParseQuery внутри
// r.ParseForm() вернёт ошибку escape-последовательности, не связанную с MaxBytesReader.
func TestParseFormGenericErrorReturns400(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/settings/theme", strings.NewReader("theme=dark"))
	r.URL.RawQuery = "a=%zz"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")

	rec := httptest.NewRecorder()
	h.themeSwitch(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (общая ошибка ParseForm — не про размер тела): %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
