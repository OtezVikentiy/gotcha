package web_test

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

// TestCoverSameOriginGuards — все POST-обработчики CH-страниц проверяют
// sameOrigin ДО обращения к сервисам, поэтому запрос без Origin отвергается 403
// даже на стенде без ClickHouse (newStack). Покрывает sameOrigin-ветку каждого.
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

// TestCoverBadPathIDs — read-обработчики с невалидным {id}/{monitorID} в пути
// отдают 404 (parse-ветка), не касаясь сервисов — безопасно на newStack.
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
		resp := getWithCookie(t, s.srv, p, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s (bad id) status = %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestCoverNilServiceGuards — на newStack сервисы Trace/PerfIssues/Regressions/
// ProfileRegressions/Metrics/Profiles/Uptime/UptimeQuery не заведены; их
// read-роуты отвечают 404 (nil-guard срабатывает до обращения к CH/PG). Проект
// не обязан существовать — nil-guard проверяется раньше доступа. Для traces id —
// строка, парсинга нет.
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
		// Оба трейс-роута защищены nil-guard'ом на h.Trace: waterfall и flame
		// на стенде без трейсинга отдают 404, а не падают nil-разыменованием.
		"/traces/some-trace-id",
		"/traces/some-trace-id/flame",
		"/projects/999999/performance/GET%20%2Fapi%2Fusers",
		// Read-роуты подсистемы мониторинга (h.Uptime/h.UptimeQuery nil на
		// newStack) — guard до CanAccessProject/requireProjectRole/requireOrgRole,
		// поэтому 404, а не паника. /alerts заведён Alerts на этом стенде, но
		// несуществующий проект всё равно даёт 404 через requireProjectRole —
		// сам guard проверяется на прочих роутах этого списка.
		"/projects/999999/monitors",
		"/projects/999999/incidents",
		"/projects/999999/maintenance",
		"/projects/999999/alerts",
		"/projects/999999/statuspages",
		"/orgs/999999/probes",
	}
	for _, p := range getPaths {
		resp := getWithCookie(t, s.srv, p, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s (nil service) status = %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestCoverNilServiceGuardsPOST — POST-обработчики подсистемы мониторинга
// (h.Uptime nil на newStack) отдают 404 через nil-guard, а не паникуют. Guard
// стоит до requireProjectRole, поэтому несуществующий проект/монитор всё равно
// доходит до него. sameOrigin выполняется через referer=srv.URL в postForm.
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
		resp := postForm(t, s.srv, p, url.Values{}, s.srv.URL, cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s (nil service) status = %d, want 404", p, resp.StatusCode)
		}
	}
}
