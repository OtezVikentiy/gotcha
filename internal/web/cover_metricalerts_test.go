package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverMetricAlertValidation — недокрытые ветки metricAlertCreate/Delete:
// нефинитный порог, неположительное окно, невалидное правило → 422; member →
// 404; delete нечисловой rule_id → 400.
func TestCoverMetricAlertValidation(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "cover-ma-owner@example.com")
	_, memberCookie := orgSettingsRegister(t, s.auth, "cover-ma-member@example.com")
	memberID, _ := orgSettingsRegister(t, s.auth, "cover-ma-member2@example.com")
	o, err := s.org.CreateOrg(ctx, "cover-ma-co", "Cover MA", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(ctx, o.ID, "cover-ma-proj", "Cover MA Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/metrics/alerts"

	valid := func() url.Values {
		return url.Values{
			"metric_name": {"http.requests"}, "aggregation": {"avg"}, "comparator": {"gt"},
			"threshold": {"100"}, "window_seconds": {"300"},
		}
	}

	// member → 404 (requireProjectRole).
	resp := postForm(t, s.srv, base, valid(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST metric alert (member) status = %d, want 404", resp.StatusCode)
	}

	// Нефинитный порог (NaN) → 422.
	bad := valid()
	bad.Set("threshold", "NaN")
	resp = postForm(t, s.srv, base, bad, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST metric alert (NaN threshold) status = %d, want 422", resp.StatusCode)
	}

	// Неположительное окно → 422.
	bad = valid()
	bad.Set("window_seconds", "0")
	resp = postForm(t, s.srv, base, bad, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST metric alert (window=0) status = %d, want 422", resp.StatusCode)
	}

	// Невалидное правило (пустое имя метрики) → 422 (ErrInvalidRule).
	bad = valid()
	bad.Set("metric_name", "")
	resp = postForm(t, s.srv, base, bad, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST metric alert (invalid rule) status = %d, want 422", resp.StatusCode)
	}

	// Валидное создание → 303.
	resp = postForm(t, s.srv, base, valid(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST metric alert (valid) status = %d, want 303", resp.StatusCode)
	}

	// Delete нечисловой rule_id → 400.
	resp = postForm(t, s.srv, base+"/delete", url.Values{"rule_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST metric alert delete (bad rule_id) status = %d, want 400", resp.StatusCode)
	}

	// Delete member → 404.
	resp = postForm(t, s.srv, base+"/delete", url.Values{"rule_id": {"1"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST metric alert delete (member) status = %d, want 404", resp.StatusCode)
	}
}
