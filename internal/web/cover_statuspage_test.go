package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestWebStatusPageOperator — участник команды: (1) создаёт страницу — она
// создаётся с Enabled=false, ДАЖЕ если форма прислала enabled=on; (2) правит
// title существующей — проходит, но Enabled остаётся прежним, даже если
// форма прислала другое значение; (3) страница настроек доступна (200).
// Admin-путь (полная форма) закреплён существующими тестами
// cover_statuspage_test.go / statuspage_test.go (спека
// cld/plans/2026-08-08-access-model-rework.md: контент оператору, публикация
// admin). Slug форма не шлёт вовсе (задача 4 плана) — прежняя проверка «slug
// не меняется оператором» отсюда убрана вместе с полем.
func TestWebStatusPageOperator(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, memberCookie := statusPageProject(t, s, "spop")
	m := statusPageMonitor(t, s, proj.ID, "spop-monitor", "https://example.com/spop")

	// Владелец создаёт опубликованную страницу.
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Op Status", Enabled: true,
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

	// (2) участник команды правит title существующей страницы, но enabled из
	// формы игнорируется — сервер сохраняет прежнее.
	updatePath := "/statuspages/" + strconv.FormatInt(sp.ID, 10)
	form := url.Values{
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
	if !got.Enabled {
		t.Fatalf("Enabled = false, want unchanged true (publication is admin-only)")
	}

	// (1) участник команды создаёт новую страницу: даже с enabled=on она
	// рождается выключенной.
	createForm := url.Values{
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
	// Матчим по Title: поля Slug у StatusPage больше нет (T5).
	var created *uptime.StatusPage
	for i := range pages {
		if pages[i].Title == "Born Disabled" {
			created = &pages[i]
		}
	}
	if created == nil {
		t.Fatalf("new page «Born Disabled» not found among %+v", pages)
	}
	if created.Enabled {
		t.Fatalf("Enabled = true, want false (operator create must not publish, form sent enabled=on)")
	}
}

// TestWebStatusPageDeletePublicationGate — A3 (security P1-3): удаление
// опубликованной страницы снимает её с публичного интернета — это
// публикационное решение, а не обычная правка контента, поэтому оператор
// без canManageProject не может удалить
// Enabled=true страницу (только Enabled=false, ещё никому не видимую).
// Admin/owner удаляет любую. Страница уже загружена loadManagedStatusPage
// (существование не секрет) → честный 403, не 404.
func TestWebStatusPageDeletePublicationGate(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, memberCookie := statusPageProject(t, s, "spdel")
	m := statusPageMonitor(t, s, proj.ID, "spdel-monitor", "https://example.com/spdel")

	// Оператор удаляет неопубликованную страницу — обычное снятие контента.
	unpub, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Unpub", Enabled: false,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create unpublished page: %v", err)
	}
	unpubPath := "/statuspages/" + strconv.FormatInt(unpub.ID, 10) + "/delete"
	resp := postForm(t, s.srv, unpubPath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator, unpublished) = %d, want 303", unpubPath, resp.StatusCode)
	}
	if _, err := s.uptime.StatusPageByID(context.Background(), unpub.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("unpublished page must be gone after operator delete, err = %v", err)
	}

	// Оператор пытается удалить опубликованную страницу — 403, страница на
	// месте (с публичного интернета не-admin её не снимает).
	pub, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Pub", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create published page: %v", err)
	}
	pubPath := "/statuspages/" + strconv.FormatInt(pub.ID, 10) + "/delete"
	resp = postForm(t, s.srv, pubPath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (operator, published) = %d, want 403: %s", pubPath, resp.StatusCode, body)
	}
	if got, err := s.uptime.StatusPageByID(context.Background(), pub.ID); err != nil || got.ID != pub.ID {
		t.Fatalf("published page must survive operator delete attempt, got %+v, err = %v", got, err)
	}

	// Admin удаляет ту же опубликованную страницу — разрешено.
	resp = postForm(t, s.srv, pubPath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (admin, published) = %d, want 303", pubPath, resp.StatusCode)
	}
	if _, err := s.uptime.StatusPageByID(context.Background(), pub.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("published page must be gone after admin delete, err = %v", err)
	}

	// Чужак (без отношения к организации) не видит даже существование
	// страницы — 404, не 403 (тот же existence-oracle, что и раньше).
	_, strangerCookie := orgSettingsRegister(t, s.auth, "spdel-stranger@example.com")
	third, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Third", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create third page: %v", err)
	}
	thirdPath := "/statuspages/" + strconv.FormatInt(third.ID, 10) + "/delete"
	resp = postForm(t, s.srv, thirdPath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (stranger) = %d, want 404", thirdPath, resp.StatusCode)
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
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Major", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK {
		t.Fatalf("GET major status page = %d, want 200: %s", status, body)
	}
}

// TestWebStatusPageCreateRateLimited — P2-2: создание доступно любому
// оператору, а сама вставка (несколько походов в PG на попытку) достаточно
// дорогая, чтобы её не штурмовать без лимита. Дешёвая мера: per-user лимит на
// создание (12/мин). 12 попыток проходят лимитер, 13-я получает 429 — перебор
// дорожает, легитимный оператор (страницы штучные) не задет.
func TestWebStatusPageCreateRateLimited(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, memberCookie := statusPageProject(t, s, "sprl")
	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

	// Лимитер проверяется ДО разбора формы и БД, поэтому засчитывает любую
	// попытку независимо от её исхода.
	form := url.Values{"title": {"Probe"}}
	for i := 1; i <= 12; i++ {
		resp := postForm(t, s.srv, settingsPath, form, s.srv.URL, memberCookie)
		code := resp.StatusCode
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d got 429, want limiter to allow first 12", i)
		}
	}
	// 13-я попытка за окно → 429.
	resp := postForm(t, s.srv, settingsPath, form, s.srv.URL, memberCookie)
	code := resp.StatusCode
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if code != http.StatusTooManyRequests {
		t.Fatalf("13th create attempt = %d, want 429 (rate limited)", code)
	}
}
