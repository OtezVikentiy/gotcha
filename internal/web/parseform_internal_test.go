package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseFormGenericErrorReturns400 — фикс-раунд 1/5 T4, находка 2: ветка
// h.parseForm, отвечающая за ОБЩУЮ ошибку разбора формы (не про превышение
// размера тела — errors.As(err, &http.MaxBytesError{}) не совпал), не имела
// теста нигде в пакете. Ошибка получена не через тело (оно маленькое,
// в пределах любого предела), а через невалидный процент-код в query
// (RawQuery выставлен напрямую в обход httptest.NewRequest, чтобы не зависеть
// от того, как её URL-парсер сам относится к "%zz") — url.ParseQuery внутри
// r.ParseForm() вернёт ошибку escape-последовательности, никак не связанную
// с http.MaxBytesReader. Хендлер — themeSwitch: публичный, без сессии и БД,
// сам вызывает h.parseForm первым делом после sameOrigin.
func TestParseFormGenericErrorReturns400(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"} // Auth нил: без cookie сессии SetTheme не зовётся
	r := httptest.NewRequest(http.MethodPost, "http://localhost/settings/theme", strings.NewReader("theme=dark"))
	r.URL.RawQuery = "a=%zz" // невалидный процент-код — url.ParseQuery упадёт не по размеру
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")

	rec := httptest.NewRecorder()
	h.themeSwitch(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (общая ошибка ParseForm — не про размер тела): %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
