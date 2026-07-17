package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

func TestResolveThemeNoUser(t *testing.T) {
	// cookie theme имеет приоритет
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "theme", Value: "light"})
	if th, _ := resolveThemeNoUser(r); th.Code != "light" {
		t.Fatalf("cookie wins: %q", th.Code)
	}
	// без cookie — дефолт system
	r2 := httptest.NewRequest("GET", "/", nil)
	if th, _ := resolveThemeNoUser(r2); th.Code != "system" {
		t.Fatalf("default: %q", th.Code)
	}
}

func TestWithThemeSetsContextAndSkipsStatic(t *testing.T) {
	h := &Handler{} // Auth nil: запросы БЕЗ сессионной cookie не трогают Auth
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = theme.FromContext(r.Context()).Code
		w.WriteHeader(200)
	})
	mw := h.withTheme(next)

	r := httptest.NewRequest("GET", "/projects", nil)
	r.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	mw.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "dark" {
		t.Fatalf("ctx theme = %q, want dark", seen)
	}

	// /static/* — миддлвара пропускает без резолвинга (остаётся дефолт)
	seen = ""
	rs := httptest.NewRequest("GET", "/static/app.css", nil)
	rs.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	mw.ServeHTTP(httptest.NewRecorder(), rs)
	if seen != "system" {
		t.Fatalf("static should skip resolve, ctx theme = %q, want system(default)", seen)
	}
}

func TestThemeSwitchSetsCookie(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"} // Auth nil: без сессии SetTheme не зовётся
	r := httptest.NewRequest("POST", "/settings/theme", strings.NewReader("theme=dark"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Referer", "http://localhost/projects")
	w := httptest.NewRecorder()
	h.themeSwitch(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "theme" && c.Value == "dark" {
			found = true
		}
	}
	if !found {
		t.Fatal("theme=dark cookie not set")
	}
	if loc := w.Header().Get("Location"); loc != "/projects" {
		t.Fatalf("redirect = %q, want /projects", loc)
	}
}

func TestThemeSwitchRejectsCrossOrigin(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost"}
	r := httptest.NewRequest("POST", "/settings/theme", strings.NewReader("theme=dark"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	h.themeSwitch(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
}

func themeCookieValue(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == themeCookie {
			return c.Value
		}
	}
	return ""
}

func TestWithThemeResolvesStoredUserTheme(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-theme-light@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetTheme(ctx, uid, "light"); err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	mw := h.withTheme(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = theme.FromContext(r.Context()).Code
	}))
	r := httptest.NewRequest("GET", "/projects", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if seen != "light" {
		t.Fatalf("resolved theme = %q, want light", seen)
	}
	if themeCookieValue(w) != "light" {
		t.Fatalf("self-heal theme cookie = %q, want light", themeCookieValue(w))
	}
}

func TestWithThemeSeedsCookieWhenUserThemeUnset(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-theme-unset@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	mw := h.withTheme(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = theme.FromContext(r.Context()).Code
	}))
	r := httptest.NewRequest("GET", "/projects", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if seen != theme.Default.Code {
		t.Fatalf("resolved theme = %q, want %q (Default)", seen, theme.Default.Code)
	}
	// cookie must be seeded so subsequent requests skip the DB path
	if themeCookieValue(w) != theme.Default.Code {
		t.Fatalf("fallback theme cookie not seeded (got %q) — would re-query every request", themeCookieValue(w))
	}
}

func TestThemeSwitchPersistsUserTheme(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	h := &Handler{Auth: svc, BaseURL: "http://localhost"}
	ctx := context.Background()
	uid, err := svc.Register(ctx, "mw-theme-switch@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/settings/theme", strings.NewReader("theme=dark"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Referer", "http://localhost/projects")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	w := httptest.NewRecorder()
	h.themeSwitch(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	code, err := svc.UserTheme(ctx, uid)
	if err != nil || code != "dark" {
		t.Fatalf("users.theme after switch = %q, err=%v; want dark", code, err)
	}
}
