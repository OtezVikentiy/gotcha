package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	resolveWithDef := func(target string, cookie string, def string) (TimeRange, *httptest.ResponseRecorder) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: rangeCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		return h.resolveTimeRange(w, r, def), w
	}
	setCookieValue := func(w *httptest.ResponseRecorder) string {
		for _, c := range w.Result().Cookies() {
			if c.Name == rangeCookie {
				return c.Value
			}
		}
		return ""
	}

	tr, w := resolve("/x?period=7d", "")
	if tr.Key != "7d" || setCookieValue(w) != "7d" {
		t.Errorf("period=7d: Key=%q cookie=%q, want 7d/7d", tr.Key, setCookieValue(w))
	}

	// "all" на странице без него откатывается на дефолт, а не на TimeRange с нулевым From/To —
	// иначе график рисовал бы пустую страницу без единой ошибки.
	tr, w = resolve("/x?period=all", "")
	if tr.Key != "24h" || setCookieValue(w) != "24h" {
		t.Errorf("period=all (def=24h): Key=%q cookie=%q, want 24h/24h", tr.Key, setCookieValue(w))
	}

	tr, w = resolve("/x?start=2026-08-01&end=2026-08-02", "")
	if !tr.Custom || setCookieValue(w) != "" {
		t.Errorf("custom: Custom=%v cookie=%q, want true/пусто", tr.Custom, setCookieValue(w))
	}

	tr, w = resolve("/x", "1h")
	if tr.Key != "1h" || setCookieValue(w) != "" {
		t.Errorf("cookie=1h: Key=%q cookie=%q, want 1h/без перезаписи", tr.Key, setCookieValue(w))
	}

	tr, _ = resolve("/x", "bogus")
	if tr.Key != "24h" {
		t.Errorf("cookie=bogus: Key=%q, want 24h", tr.Key)
	}

	tr, w = resolve("/x?period=30d", "7d")
	if tr.Key != "30d" || setCookieValue(w) != "30d" {
		t.Errorf("query>cookie: Key=%q cookie=%q, want 30d/30d", tr.Key, setCookieValue(w))
	}

	// def=RangeAll — осознанный выбор вызывающего; пресет, оставленный cookie с другой
	// страницы, не должен его подменять.
	tr, _ = resolveWithDef("/x", "24h", RangeAll)
	if tr.Key != RangeAll {
		t.Errorf("def=all, cookie=24h: Key=%q, want all", tr.Key)
	}

	_, w = resolve("/x?period=7d", "")
	raw := w.Header().Get("Set-Cookie")
	if !strings.Contains(raw, "Max-Age=31536000") || !strings.Contains(raw, "SameSite=Lax") || !strings.Contains(raw, "Path=/") {
		t.Errorf("cookie attrs = %q", raw)
	}
}
