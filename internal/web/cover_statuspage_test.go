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

// TestWebStatusPageOperator — участник команды: (1) создаёт страницу — она
// создаётся с Enabled=false, ДАЖЕ если форма прислала enabled=on; (2) правит
// title существующей — проходит, но slug и Enabled остаются прежними, даже
// если форма прислала другие; (3) страница настроек доступна (200).
// Admin-путь (полная форма) закреплён существующими тестами
// cover_statuspage_test.go / statuspage_test.go (спека
// cld/plans/2026-08-08-access-model-rework.md: контент оператору, публикация
// admin).
func TestWebStatusPageOperator(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, memberCookie := statusPageProject(t, s, "spop")
	m := statusPageMonitor(t, s, proj.ID, "spop-monitor", "https://example.com/spop")

	// Владелец создаёт опубликованную страницу.
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "sp-op", Title: "Op Status", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

	// (3) участник команды видит настройки.
	resp := getWithCookie(t, s.srv, settingsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (operator) = %d, want 200", settingsPath, resp.StatusCode)
	}

	// (2) участник команды правит title существующей страницы, но slug и
	// enabled из формы игнорируются — сервер сохраняет прежние.
	updatePath := "/statuspages/" + strconv.FormatInt(sp.ID, 10)
	form := url.Values{
		"slug":    {"hijack"},
		"title":   {"New"},
		"enabled": {"on"},
	}
	resp = postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator update) = %d, want 303", updatePath, resp.StatusCode)
	}
	got, err := s.uptime.StatusPageByID(context.Background(), sp.ID)
	if err != nil {
		t.Fatalf("status page by id: %v", err)
	}
	if got.Title != "New" {
		t.Fatalf("Title = %q, want %q (title is operator content)", got.Title, "New")
	}
	if got.Slug != "sp-op" {
		t.Fatalf("Slug = %q, want unchanged %q (slug change is admin-only)", got.Slug, "sp-op")
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false, want unchanged true (publication is admin-only)")
	}

	// (1) участник команды создаёт новую страницу: даже с enabled=on она
	// рождается выключенной.
	createForm := url.Values{
		"slug":    {"sp-op-new"},
		"title":   {"Born Disabled"},
		"enabled": {"on"},
	}
	resp = postForm(t, s.srv, settingsPath, createForm, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator create) = %d, want 303", settingsPath, resp.StatusCode)
	}
	pages, err := s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	var created *uptime.StatusPage
	for i := range pages {
		if pages[i].Slug == "sp-op-new" {
			created = &pages[i]
		}
	}
	if created == nil {
		t.Fatalf("new page sp-op-new not found among %+v", pages)
	}
	if created.Enabled {
		t.Fatalf("Enabled = true, want false (operator create must not publish, form sent enabled=on)")
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
