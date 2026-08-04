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

	// "all" используется, но НЕ записывается.
	tr, w = resolve("/x?period=all", "")
	if tr.Key != RangeAll || setCookieValue(w) != "" {
		t.Errorf("period=all: Key=%q cookie=%q, want all/пусто", tr.Key, setCookieValue(w))
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
