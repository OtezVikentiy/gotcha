package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveTimeRange — резолв окна времени с «липкостью» (№25): явный query
// важнее cookie и записывает выбор; без query берётся cookie; без обоих —
// дефолт страницы. В cookie попадают только пресеты (ни custom, ни "all").
func TestResolveTimeRange(t *testing.T) {
	h := &Handler{}

	resolve := func(target string, cookie string) (TimeRange, *httptest.ResponseRecorder) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: rangeCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		return h.resolveTimeRange(w, r, "24h"), w
	}
	setCookieValue := func(w *httptest.ResponseRecorder) string {
		for _, c := range w.Result().Cookies() {
			if c.Name == rangeCookie {
				return c.Value
			}
		}
		return ""
	}

	// Явный пресет → используется и записывается.
	tr, w := resolve("/x?period=7d", "")
	if tr.Key != "7d" || setCookieValue(w) != "7d" {
		t.Errorf("period=7d: Key=%q cookie=%q, want 7d/7d", tr.Key, setCookieValue(w))
	}

	// "all" на странице, которая его не предлагает (def="24h", как в этом
	// тесте — только issues.go зовёт resolveTimeRange с def=RangeAll),
	// откатывается на дефолт страницы, а не тихо отдаёт TimeRange с нулевыми
	// From/To (находка P1-6: график с нулевым окном рисовал пустую страницу
	// без единой ошибки — см. parseTimeRange). Ведёт себя как любой другой
	// нераспознанный на этой странице period ("bogus" и т.п.): дефолт
	// становится действующим пресетом и, как обычный пресет, запоминается.
	tr, w = resolve("/x?period=all", "")
	if tr.Key != "24h" || setCookieValue(w) != "24h" {
		t.Errorf("period=all (def=24h): Key=%q cookie=%q, want 24h/24h", tr.Key, setCookieValue(w))
	}

	// Custom-диапазон используется, но НЕ записывается.
	tr, w = resolve("/x?start=2026-08-01&end=2026-08-02", "")
	if !tr.Custom || setCookieValue(w) != "" {
		t.Errorf("custom: Custom=%v cookie=%q, want true/пусто", tr.Custom, setCookieValue(w))
	}

	// Без query дефолт приходит из cookie.
	tr, w = resolve("/x", "1h")
	if tr.Key != "1h" || setCookieValue(w) != "" {
		t.Errorf("cookie=1h: Key=%q cookie=%q, want 1h/без перезаписи", tr.Key, setCookieValue(w))
	}

	// Невалидный cookie молча игнорируется → дефолт страницы.
	tr, _ = resolve("/x", "bogus")
	if tr.Key != "24h" {
		t.Errorf("cookie=bogus: Key=%q, want 24h", tr.Key)
	}

	// Явный query перебивает cookie и перезаписывает его.
	tr, w = resolve("/x?period=30d", "7d")
	if tr.Key != "30d" || setCookieValue(w) != "30d" {
		t.Errorf("query>cookie: Key=%q cookie=%q, want 30d/30d", tr.Key, setCookieValue(w))
	}

	// Атрибуты cookie: год жизни, SameSite=Lax, путь /.
	_, w = resolve("/x?period=7d", "")
	raw := w.Header().Get("Set-Cookie")
	if !strings.Contains(raw, "Max-Age=31536000") || !strings.Contains(raw, "SameSite=Lax") || !strings.Contains(raw, "Path=/") {
		t.Errorf("cookie attrs = %q", raw)
	}
}
