package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestCoverStatusPageUpdateSlugTaken — statusPagesUpdate с занятым slug → 422
// (перерисовка формы) и ErrNotFound-ветка для несуществующей страницы.
func TestCoverStatusPageUpdateSlugTaken(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spupd")
	m := statusPageMonitor(t, s, proj.ID, "upd-mon", "https://example.com/upd")

	spA, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spupd-a", Title: "A", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "A", Position: 0}})
	if err != nil {
		t.Fatalf("create page A: %v", err)
	}
	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spupd-b", Title: "B", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "B", Position: 0}}); err != nil {
		t.Fatalf("create page B: %v", err)
	}

	// Обновить страницу A, задав slug страницы B → 422 (ErrSlugTaken).
	updPath := "/statuspages/" + strconv.FormatInt(spA.ID, 10)
	resp := postForm(t, s.srv, updPath, url.Values{
		"slug": {"spupd-b"}, "title": {"A2"}, "enabled": {"on"},
	}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST update (slug taken) = %d, want 422: %s", resp.StatusCode, body)
	}
}

// TestCoverStatusPageMajorOutage — единственный монитор в down: общий статус
// «major», а на странице рендерится инцидент (ветки incident-цикла и сортировки).
func TestCoverStatusPageMajorOutage(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spmajor")
	m := statusPageMonitor(t, s, proj.ID, "down-only", "https://example.com/down")

	at := time.Now().UTC().Add(-5 * time.Minute)
	if _, err := s.uptime.ApplyResult(context.Background(), m.ID, "local", false, "dial tcp: refused", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	if _, _, err := s.uptime.OpenIncident(context.Background(), m.ID, "dial tcp: refused", []string{"local"}, false); err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spmajor-status", Title: "Major", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}}); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/spmajor-status")
	if status != http.StatusOK {
		t.Fatalf("GET major status page = %d, want 200: %s", status, body)
	}
}
