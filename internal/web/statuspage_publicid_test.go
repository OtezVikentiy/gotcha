package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestWebStatusPageResolvesByPublicID(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppid-ok")
	m := statusPageMonitor(t, s, proj.ID, "pid-ok-monitor", "https://example.com/pid-ok")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "PID OK", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK {
		t.Fatalf("GET /status/%s = %d, want 200: %s", sp.PublicID, status, body)
	}
	if !strings.Contains(body, "PID OK") {
		t.Fatalf("public page missing title: %s", body)
	}
}

func TestWebStatusPageLegacySlugRedirects(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppid-legacy")
	m := statusPageMonitor(t, s, proj.ID, "pid-legacy-monitor", "https://example.com/pid-legacy")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Legacy Target", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ($1, $2)`,
		"oldslug", sp.ID); err != nil {
		t.Fatalf("insert legacy redirect: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/status/oldslug", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("GET /status/oldslug = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/status/"+sp.PublicID {
		t.Fatalf("Location = %q, want %q", got, "/status/"+sp.PublicID)
	}
}

func TestWebStatusPageLegacySlugOfDisabledPage404(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppid-off")
	m := statusPageMonitor(t, s, proj.ID, "pid-off-monitor", "https://example.com/pid-off")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Legacy Disabled", Enabled: false,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ($1, $2)`,
		"oldslug-off", sp.ID); err != nil {
		t.Fatalf("insert legacy redirect: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/oldslug-off")
	if status != http.StatusNotFound {
		t.Fatalf("GET /status/oldslug-off (disabled target) = %d, want 404: %s", status, body)
	}
}

func TestWebStatusPageUnknownPublicID404(t *testing.T) {
	s := newStatusPageStack(t)
	status, body := getAnon(t, s.srv, "/status/p_deadbeefdeadbeefdeadbeef")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown public_id = %d, want 404: %s", status, body)
	}
}

func TestWebStatusPageNonsenseKey404(t *testing.T) {
	s := newStatusPageStack(t)
	status, body := getAnon(t, s.srv, "/status/nonsense")
	if status != http.StatusNotFound {
		t.Fatalf("GET /status/nonsense = %d, want 404: %s", status, body)
	}
}
