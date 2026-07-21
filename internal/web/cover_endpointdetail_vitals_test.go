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

// TestWebEndpointDetailVitalsAndParams добивает ветки endpointDetail, которые
// не задействует базовый TestWebEndpointDetail: панель Web Vitals (у транзакции
// есть замеры lcp/inp/cls за окно), не-дефолтное окно ?period=1h
// (perfPeriodWindow), фильтр ?environment и происхождение ?from=web-vitals
// (endpointOrigin). Всё через реальный CH (perfStack), как остальные perf-тесты.
func TestWebEndpointDetailVitalsAndParams(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "epvitals-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "epvitals-co", "EPVitals Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "epvitals-proj", "EPVitals Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const tx = "GET /api/checkout"
	base := time.Now().UTC().Add(-15 * time.Minute)
	// Транзакции с web-vitals: lcp/inp/cls присутствуют → vitalsPanel вернёт
	// непустую панель (иначе секция не рендерится и её ветки не покрываются).
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:      fmt.Sprintf("epv-%02d", i),
			SpanID:       fmt.Sprintf("epvspan-%02d", i),
			Name:         tx,
			Op:           "http.server",
			Status:       "ok",
			Start:        at,
			End:          at.Add(40 * time.Millisecond),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": 2400, "inp": 180, "cls": 0.05},
		})
	}
	s.flush(t)

	txPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape(tx)

	// Не-дефолтное окно + фильтр окружения + происхождение из раздела web-vitals.
	q := "?period=1h&environment=production&from=web-vitals"
	resp := getWithCookie(t, s.srv, txPath+q, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", txPath+q, resp.StatusCode, body)
	}
	// Панель vitals присутствует (значение lcp p75 = 2.40s) и заголовок эндпойнта.
	for _, want := range []string{tx, "2.40s"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q (vitals panel not rendered?): %s", txPath+q, want, body)
		}
	}

	// Дефолтное окружение (без ?environment) тоже 200 — ветка пустого фильтра.
	resp = getWithCookie(t, s.srv, txPath+"?period=1h", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?period=1h status = %d, want 200", txPath, resp.StatusCode)
	}
}
