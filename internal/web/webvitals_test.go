package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestWebVitalsOverview — owner видит страницы с p75 LCP/INP/CLS, цветными
// бейджами рейтинга и человекочитаемым форматированием (мс/CLS); пустой проект
// отдаёт «no web vitals», не падает; чужой проект → 404.
func TestWebVitalsOverview(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "wvlist-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "wvlist-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "wvlist-co", "WVList Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "wvlist-proj", "WVList Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	empty, err := s.org.CreateProject(context.Background(), o.ID, "wvlist-empty", "WVList Empty", "go")
	if err != nil {
		t.Fatalf("create empty project: %v", err)
	}

	base := time.Now().UTC().Add(-20 * time.Minute)
	// «GET /home» production — 4 pageload-транзакции с постоянными lcp=2500
	// (p75=2500, граница good) и cls=0.05 (good).
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:      fmt.Sprintf("wv-home-%02d", i),
			SpanID:       fmt.Sprintf("wv-homespan-%02d", i),
			Name:         "GET /home",
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": 2500, "cls": 0.05},
		})
	}
	// «GET /slow» production — 6 транзакций lcp=5000 (p75=5000 → poor). Замеров
	// больше, поэтому идёт первой при сортировке по числу замеров.
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:      fmt.Sprintf("wv-slow-%02d", i),
			SpanID:       fmt.Sprintf("wv-slowspan-%02d", i),
			Name:         "GET /slow",
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": 5000},
		})
	}
	s.flush(t)

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/web-vitals"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{
		"GET /home", "GET /slow",
		"2.50s",   // lcp home p75
		"5.00s",   // lcp slow p75
		"0.05",    // cls home p75 (безразмерный, 2 знака — не мс)
		"badge-good",
		"badge-danger",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}

	// Пустой проект: «no web vitals», не падает.
	emptyPath := "/projects/" + strconv.FormatInt(empty.ID, 10) + "/web-vitals"
	resp = getWithCookie(t, s.srv, emptyPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (empty) status = %d, want 200: %s", emptyPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Пока нет Web Vitals") {
		t.Fatalf("GET %s (empty) missing 'no web vitals': %s", emptyPath, body)
	}

	// Чужой проект → 404.
	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebVitalsEndpointPanel — на странице эндпойнта с web vitals есть панель
// «Web Vitals» с p75 всех пяти показателей, рейтинг-бейджами и SVG-графиками;
// у эндпойнта без vitals панели нет.
func TestWebVitalsEndpointPanel(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "wvpanel-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "wvpanel-co", "WVPanel Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "wvpanel-proj", "WVPanel Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	base := time.Now().UTC().Add(-20 * time.Minute)
	// «GET /home» — pageload с полным набором vitals (все good, постоянные
	// значения → точный p75).
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("wvp-home-%02d", i),
			SpanID:      fmt.Sprintf("wvp-homespan-%02d", i),
			Name:        "GET /home",
			Op:          "pageload",
			Status:      "ok",
			Start:       at,
			End:         at.Add(time.Second),
			Environment: "production",
			Measurements: map[string]float64{
				"lcp": 2500, "inp": 150, "cls": 0.05, "fcp": 1500, "ttfb": 700,
			},
		})
	}
	// «GET /api/orders» — обычная http.server-транзакция без measurements
	// (панели быть не должно).
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("wvp-order-%02d", i),
			SpanID:      fmt.Sprintf("wvp-orderspan-%02d", i),
			Name:        "GET /api/orders",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(20 * time.Millisecond),
			Environment: "production",
		})
	}
	s.flush(t)

	// Эндпойнт с vitals: панель есть.
	homePath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape("GET /home")
	resp := getWithCookie(t, s.srv, homePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", homePath, resp.StatusCode, body)
	}
	for _, want := range []string{
		"<h2>Web Vitals</h2>",
		"LCP", "INP", "CLS", "FCP", "TTFB",
		"2.50s", // lcp
		"150ms", // inp
		"0.05",  // cls
		"1.50s", // fcp
		"700ms", // ttfb
		"badge-good",
		"<svg",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (vitals panel) missing %q: %s", homePath, want, body)
		}
	}

	// Эндпойнт без vitals: панели нет.
	orderPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape("GET /api/orders")
	resp = getWithCookie(t, s.srv, orderPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", orderPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "<h2>Web Vitals</h2>") {
		t.Fatalf("GET %s (no vitals) must not render Web Vitals panel: %s", orderPath, body)
	}

	// Vitals есть только в production. При фильтре environment=staging панели
	// быть НЕ должно (раньше рендерилась с прочерками, т.к. общий p75 брался без
	// учёта окружения).
	stagingPath := homePath + "?environment=staging"
	resp = getWithCookie(t, s.srv, stagingPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", stagingPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "<h2>Web Vitals</h2>") {
		t.Fatalf("GET %s (environment=staging) must not render Web Vitals panel: %s", stagingPath, body)
	}

	// А при явном environment=production панель есть с реальными значениями.
	prodPath := homePath + "?environment=production"
	resp = getWithCookie(t, s.srv, prodPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", prodPath, resp.StatusCode, body)
	}
	for _, want := range []string{"<h2>Web Vitals</h2>", "2.50s", "0.05"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (environment=production) missing %q: %s", prodPath, want, body)
		}
	}
}

// TestWebVitalsPageHasNavLink — страница web-vitals доступна из навигации рядом
// с Performance (ссылка на /web-vitals присутствует на списке эндпойнтов).
func TestWebVitalsPageHasNavLink(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "wvnav-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "wvnav-co", "WVNav Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "wvnav-proj", "WVNav Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	perfPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance"
	wvPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/web-vitals"

	resp := getWithCookie(t, s.srv, perfPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", perfPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), wvPath) {
		t.Fatalf("GET %s missing web-vitals nav link %q: %s", perfPath, wvPath, body)
	}
}
