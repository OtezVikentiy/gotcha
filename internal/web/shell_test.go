package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
)

func TestWithShellSkipsStaticAndAnonymous(t *testing.T) {
	h := &Handler{} // Auth/Org nil: запросы БЕЗ сессионной cookie не трогают их

	var seen nav.Shell
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nav.FromContext(r.Context())
		w.WriteHeader(200)
	})
	mw := h.withShell(next)

	// /static/* — миддлвара пропускает без резолвинга.
	seen = nav.Shell{}
	rs := httptest.NewRequest("GET", "/static/app.css", nil)
	mw.ServeHTTP(httptest.NewRecorder(), rs)
	if seen.Area != "" {
		t.Fatalf("static should skip resolve, got area = %q", seen.Area)
	}

	// Запрос без сессии — тоже без shell, и не паникует на nil Auth/Org.
	seen = nav.Shell{}
	r := httptest.NewRequest("GET", "/projects/1/issues", nil)
	mw.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Area != "" {
		t.Fatalf("anonymous request should skip resolve, got area = %q", seen.Area)
	}
}

func TestProjectIDFromPath(t *testing.T) {
	cases := map[string]int64{
		"/projects/7/issues": 7,
		"/projects/7":        7,
		"/issues/9":          0,
		"/orgs/5/teams":      0,
		"/profile":           0,
		"/":                  0,
	}
	for path, want := range cases {
		if got := projectIDFromPath(path); got != want {
			t.Errorf("projectIDFromPath(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestOrgIDFromPath(t *testing.T) {
	cases := map[string]int64{
		"/orgs/5/teams":      5,
		"/orgs/5":            5,
		"/projects/7/issues": 0,
		"/profile":           0,
		"/":                  0,
	}
	for path, want := range cases {
		if got := orgIDFromPath(path); got != want {
			t.Errorf("orgIDFromPath(%q) = %d, want %d", path, got, want)
		}
	}
}
