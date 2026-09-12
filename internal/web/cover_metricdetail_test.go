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
	s.seedGauge(t, proj.ID, "cpu.usage", "prod", 0.4, map[string]string{"host": "h1"})
	s.seedGauge(t, proj.ID, "cpu.usage", "prod", 0.6, map[string]string{"host": "h1"})

	detail := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/" + url.PathEscape("cpu.usage")

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

	resp = getWithCookie(t, s.srv, detail, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (defaults) status = %d, want 200", detail, resp.StatusCode)
	}

	cq := "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, detail+cq, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", detail+cq, resp.StatusCode, cbody)
	}
	if !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("GET %s did not render custom range selected: %s", detail+cq, cbody)
	}

	missing := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/" + url.PathEscape("nope.metric")
	resp = getWithCookie(t, s.srv, missing, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (missing metric) status = %d, want 404", missing, resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, detail, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", detail, resp.StatusCode)
	}
}
