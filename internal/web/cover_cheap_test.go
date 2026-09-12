package web_test

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

func TestCoverSameOriginGuards(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "cheap-sameorigin@example.com")

	postPaths := []string{
		"/projects/1/monitors",
		"/monitors/1",
		"/monitors/1/pause",
		"/monitors/1/resume",
		"/monitors/1/delete",
		"/projects/1/maintenance",
		"/projects/1/maintenance/delete",
		"/projects/1/statuspages",
		"/statuspages/1",
		"/statuspages/1/delete",
		"/perf-issues/1/status",
		"/projects/1/metrics/alerts",
		"/projects/1/metrics/alerts/delete",
	}
	for _, p := range postPaths {
		resp := postForm(t, s.srv, p, url.Values{}, "", cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (no origin) status = %d, want 403", p, resp.StatusCode)
		}
	}
}

func TestCoverBadPathIDs(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "cheap-badid@example.com")

	getPaths := []string{
		"/monitors/not-a-number",
		"/monitors/not-a-number/edit",
		"/perf-issues/not-a-number",
		"/projects/not-a-number/performance",
		"/projects/not-a-number/perf-issues",
		"/projects/not-a-number/web-vitals",
		"/projects/not-a-number/regressions",
		"/projects/not-a-number/profiles",
		"/projects/not-a-number/profile-regressions",
		"/projects/not-a-number/metrics",
		"/projects/not-a-number/monitors",
		"/projects/not-a-number/monitors/new",
		"/projects/not-a-number/maintenance",
		"/projects/not-a-number/statuspages",
		"/projects/not-a-number/incidents",
	}
	for _, p := range getPaths {
		assertRouteRegistered(t, s, http.MethodGet, p)

		resp := getWithCookie(t, s.srv, p, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s (bad id) status = %d, want 404", p, resp.StatusCode)
		}
	}
}

func assertRouteRegistered(t *testing.T, s *stack, method, path string) {
	t.Helper()
	pattern := s.h.RoutePattern(method, path)
	if pattern == "" || pattern == "/" {
		t.Fatalf("%s %s: маршрут не зарегистрирован (шаблон %q) — 404 приходит "+
			"от catch-all, и тест проверял бы отсутствие маршрута, а не работу обработчика",
			method, path, pattern)
	}
}

func TestCoverNilServiceGuards(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "cheap-nilguard@example.com")

	getPaths := []string{
		"/projects/999999/performance",
		"/projects/999999/perf-issues",
		"/projects/999999/web-vitals",
		"/projects/999999/regressions",
		"/projects/999999/profiles",
		"/projects/999999/profile-regressions",
		"/projects/999999/metrics",
		"/traces/some-trace-id",
		"/traces/some-trace-id/flame",
		"/projects/999999/performance/GET%20%2Fapi%2Fusers",
		"/projects/999999/monitors",
		"/projects/999999/incidents",
		"/projects/999999/maintenance",
		"/projects/999999/alerts",
		"/projects/999999/statuspages",
		"/orgs/999999/probes",
	}
	for _, p := range getPaths {
		assertRouteRegistered(t, s, http.MethodGet, p)
		resp := getWithCookie(t, s.srv, p, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s (nil service) status = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestCoverNilServiceGuardsPOST(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "cheap-nilguard-post@example.com")

	postPaths := []string{
		"/projects/999999/monitors",
		"/monitors/999999",
		"/monitors/999999/pause",
		"/monitors/999999/resume",
		"/monitors/999999/delete",
		"/monitors/999999/heartbeat/regenerate",
		"/projects/999999/maintenance",
		"/projects/999999/maintenance/delete",
		"/projects/999999/statuspages",
		"/statuspages/999999",
		"/statuspages/999999/delete",
		"/orgs/999999/probes",
		"/orgs/999999/probes/revoke",
	}
	for _, p := range postPaths {
		assertRouteRegistered(t, s, http.MethodPost, p)
		resp := postForm(t, s.srv, p, url.Values{}, s.srv.URL, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s (nil service) status = %d, want 404", p, resp.StatusCode)
		}
	}
}
