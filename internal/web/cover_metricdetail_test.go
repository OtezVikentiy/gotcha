package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestWebMetricDetail покрывает страницу метрики GET /projects/{id}/metrics/{name}
// (metricDetail): существующие тесты трогают только список. Гоняем полный
// render-путь ряда (Series/Labels/Environments) с не-дефолтным окном ?period=1h,
// агрегацией ?agg, фильтром окружения и меткой ?label_key/?label_value, а также
// ветки «нет такой метрики» → 404 и чужой проект → 404.
func TestWebMetricDetail(t *testing.T) {
	s := newMetricsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "mdetail-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "mdetail-outsider@example.com")

	o, err := s.org.CreateOrg(ctx, "mdetail-co", "MDetail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(ctx, o.ID, "mdetail-proj", "MDetail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Две точки одного gauge в prod с меткой host — даёт непустой ряд, список
	// меток (host) и список окружений (prod).
	s.seedGauge(t, proj.ID, "cpu.usage", "prod", 0.4, map[string]string{"host": "h1"})
	s.seedGauge(t, proj.ID, "cpu.usage", "prod", 0.6, map[string]string{"host": "h1"})

	detail := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/" + url.PathEscape("cpu.usage")

	// Полный набор параметров: не-дефолтное окно, агрегация, окружение, метка.
	q := "?period=1h&agg=max&environment=prod&label_key=host&label_value=h1"
	resp := getWithCookie(t, s.srv, detail+q, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", detail+q, resp.StatusCode, body)
	}
	for _, want := range []string{"cpu.usage", "<svg"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", detail+q, want, body)
		}
	}

	// Дефолтные параметры (без query) — тоже 200.
	resp = getWithCookie(t, s.srv, detail, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (defaults) status = %d, want 200", detail, resp.StatusCode)
	}

	// Несуществующая метрика → 404.
	missing := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/" + url.PathEscape("nope.metric")
	resp = getWithCookie(t, s.srv, missing, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (missing metric) status = %d, want 404", missing, resp.StatusCode)
	}

	// Чужой проект → 404.
	resp = getWithCookie(t, s.srv, detail, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", detail, resp.StatusCode)
	}
}
