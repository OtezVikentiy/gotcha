package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestResolveLocaleNoUser(t *testing.T) {
	// cookie lang имеет приоритет
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "en-US,en")
	r.AddCookie(&http.Cookie{Name: "lang", Value: "ru"})
	if loc, _ := resolveLocaleNoUser(r); loc.Code != "ru" {
		t.Fatalf("cookie wins: %q", loc.Code)
	}
	// без cookie — Accept-Language
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Accept-Language", "en-US,en")
	if loc, _ := resolveLocaleNoUser(r2); loc.Code != "en" {
		t.Fatalf("accept-language: %q", loc.Code)
	}
	// пусто — дефолт ru
	r3 := httptest.NewRequest("GET", "/", nil)
	if loc, _ := resolveLocaleNoUser(r3); loc.Code != "ru" {
		t.Fatalf("default: %q", loc.Code)
	}
}

func TestWithLocaleSetsContextAndSkipsStatic(t *testing.T) {
	h := &Handler{} // Auth nil: запросы БЕЗ сессионной cookie не трогают Auth
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = i18n.FromContext(r.Context()).Code
		w.WriteHeader(200)
	})
	mw := h.withLocale(next)

	r := httptest.NewRequest("GET", "/projects", nil)
	r.Header.Set("Accept-Language", "en")
	mw.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "en" {
		t.Fatalf("ctx locale = %q, want en", seen)
	}

	// /static/* — миддлвара пропускает без резолвинга (остаётся дефолт)
	seen = ""
	rs := httptest.NewRequest("GET", "/static/app.css", nil)
	rs.Header.Set("Accept-Language", "en")
	mw.ServeHTTP(httptest.NewRecorder(), rs)
	if seen != "ru" {
		t.Fatalf("static should skip resolve, ctx locale = %q, want ru(default)", seen)
	}
}

func TestLocaleSwitchSetsCookie(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"} // Auth nil: без сессии SetLocale не зовётся
	r := httptest.NewRequest("POST", "/settings/locale", strings.NewReader("lang=en"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Referer", "http://localhost/projects")
	w := httptest.NewRecorder()
	h.localeSwitch(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "lang" && c.Value == "en" {
			found = true
		}
	}
	if !found {
		t.Fatal("lang=en cookie not set")
	}
	if loc := w.Header().Get("Location"); loc != "/projects" {
		t.Fatalf("redirect = %q, want /projects", loc)
	}
}

func TestSafeRedirect(t *testing.T) {
	base := "http://localhost"
	cases := map[string]string{
		"http://localhost/projects":        "/projects",
		"http://localhost/issues?status=x": "/issues?status=x",
		"http://localhost//evil.com/x":     "/",
		"http://localhost/\\evil.com":      "/",
		"http://evil.com/x":                "/",
		"":                                 "/",
	}
	for ref, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if ref != "" {
			r.Header.Set("Referer", ref)
		}
		if got := safeRedirect(r, base); got != want {
			t.Fatalf("safeRedirect(%q) = %q, want %q", ref, got, want)
		}
	}
}

func langCookieValue(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "lang" {
			return c.Value
		}
	}
	return ""
}

func TestWithLocaleResolvesStoredUserLocale(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-en@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetLocale(ctx, uid, "en"); err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	mw := h.withLocale(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = i18n.FromContext(r.Context()).Code
	}))
	r := httptest.NewRequest("GET", "/projects", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if seen != "en" {
		t.Fatalf("resolved locale = %q, want en", seen)
	}
	if langCookieValue(w) != "en" {
		t.Fatalf("self-heal lang cookie = %q, want en", langCookieValue(w))
	}
}

func TestWithLocaleSeedsCookieWhenUserLocaleUnset(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-unset@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	mw := h.withLocale(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = i18n.FromContext(r.Context()).Code
	}))
	r := httptest.NewRequest("GET", "/projects", nil)
	r.Header.Set("Accept-Language", "en")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if seen != "en" {
		t.Fatalf("resolved locale = %q, want en (Accept-Language fallback)", seen)
	}
	// cookie must be seeded so subsequent requests skip the DB path
	if langCookieValue(w) != "en" {
		t.Fatalf("fallback lang cookie not seeded (got %q) — would re-query every request", langCookieValue(w))
	}
}

func TestLocaleSwitchPersistsUserLocale(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-switch@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/settings/locale", strings.NewReader("lang=en"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Referer", "http://localhost/projects")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	h.localeSwitch(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	code, err := svc.UserLocale(ctx, uid)
	if err != nil || code != "en" {
		t.Fatalf("users.locale after switch = %q, err=%v; want en", code, err)
	}
}

func TestLocaleSwitchRejectsCrossOrigin(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"}
	r := httptest.NewRequest("POST", "/settings/locale", strings.NewReader("lang=en"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	h.localeSwitch(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
}
