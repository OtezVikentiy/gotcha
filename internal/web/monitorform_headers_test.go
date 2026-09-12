package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func mustKeyring(t *testing.T, raw string) secretbox.Keyring {
	t.Helper()
	ring, err := secretbox.NewKeyring(raw, "")
	if err != nil {
		t.Fatalf("NewKeyring(%q): %v", raw, err)
	}
	return ring
}

func createHeaderMonitor(t *testing.T, s *monitorFormStack, projectID int64, target string, headers map[string]string) uptime.Monitor {
	t.Helper()
	cfg, err := json.Marshal(uptime.HTTPConfig{Method: "GET", URL: target, Headers: headers})
	if err != nil {
		t.Fatalf("marshal http config: %v", err)
	}
	m := baseMonitor(projectID, "Header monitor")
	m.Config = cfg
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create header monitor: %v", err)
	}
	return created
}

func httpHeaderOf(t *testing.T, s *monitorFormStack, monitorID int64, name string) string {
	t.Helper()
	m, err := s.uptime.Get(context.Background(), monitorID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	var c uptime.HTTPConfig
	if err := json.Unmarshal(m.Config, &c); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return c.Headers[name]
}

func httpURLOf(t *testing.T, s *monitorFormStack, monitorID int64) string {
	t.Helper()
	m, err := s.uptime.Get(context.Background(), monitorID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	var c uptime.HTTPConfig
	if err := json.Unmarshal(m.Config, &c); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return c.URL
}

func operatorUpdateForm(target, headers string) url.Values {
	return url.Values{
		"name":               {"Header monitor"},
		"kind":               {"http"},
		"http_method":        {"GET"},
		"http_url":           {target},
		"http_headers":       {headers},
		"interval_seconds":   {"60"},
		"timeout_seconds":    {"10"},
		"fail_threshold":     {"1"},
		"recovery_threshold": {"1"},
		"consensus":          {"majority"},
		"regions":            {"local"},
	}
}

func TestWebMonitorEditOperatorMasksHeaderValues(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, ownerCookie, memberCookie := ownerAndMember(t, s, "monhdrmask")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	editPath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/edit"

	resp := getWithCookie(t, s.srv, editPath, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (operator) status = %d, want 200: %s", editPath, resp.StatusCode, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Authorization") {
		t.Errorf("GET %s (operator) missing header name: %s", editPath, bodyStr)
	}
	if strings.Contains(bodyStr, "Bearer supersecret") {
		t.Errorf("GET %s (operator) leaks raw header value: %s", editPath, bodyStr)
	}

	resp = getWithCookie(t, s.srv, editPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", editPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Bearer supersecret") {
		t.Errorf("GET %s (owner) missing raw header value: %s", editPath, body)
	}
}

func TestWebMonitorUpdateOperatorKeepsBlankHeader(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrkeep")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api.example.com/health", "Authorization: ****")
	form.Set("name", "Header monitor renamed")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator, masked header) status = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authorization"); got != "Bearer supersecret" {
		t.Fatalf("header value = %q after masked update, want %q (must not be wiped)", got, "Bearer supersecret")
	}
}

func TestWebMonitorUpdateOperatorURLChangeBlocksHeaderExfil(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrexfil")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://evil.example.com/collect", "Authorization: ****")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (operator, URL change + masked header) status = %d, want 422: %s", updatePath, resp.StatusCode, body)
	}
	if got := httpURLOf(t, s, created.ID); got != "https://api.example.com/health" {
		t.Fatalf("URL = %q after blocked update, want unchanged", got)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authorization"); got != "Bearer supersecret" {
		t.Fatalf("header value = %q after blocked update, want unchanged", got)
	}
}

func TestWebMonitorUpdateOperatorURLChangeWithFreshHeaders(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrfresh")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api2.example.com/health", "Authorization: Bearer freshtoken")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator, URL change + fresh header) status = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	if got := httpURLOf(t, s, created.ID); got != "https://api2.example.com/health" {
		t.Fatalf("URL = %q after update, want changed", got)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authorization"); got != "Bearer freshtoken" {
		t.Fatalf("header value = %q after update, want %q", got, "Bearer freshtoken")
	}
}

func TestWebMonitorUpdateAdminRepointsFreely(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, ownerCookie, _ := ownerAndMember(t, s, "monhdradmin")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api3.example.com/health", "Authorization: Bearer supersecret")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner, URL change) status = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	if got := httpURLOf(t, s, created.ID); got != "https://api3.example.com/health" {
		t.Fatalf("URL = %q after owner update, want changed", got)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authorization"); got != "Bearer supersecret" {
		t.Fatalf("header value = %q after owner update, want kept", got)
	}
}

func TestWebMonitorUpdateOperatorURLChangeNoStoredSecretAllowed(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrnosecret")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health", nil)
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api2.example.com/health", "X-New: ")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator, URL change + blank new header, no stored secret) status = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	if got := httpURLOf(t, s, created.ID); got != "https://api2.example.com/health" {
		t.Fatalf("URL = %q after update, want changed", got)
	}
	if got := httpHeaderOf(t, s, created.ID, "X-New"); got != "" {
		t.Fatalf("header X-New = %q after update, want empty", got)
	}
}

func TestWebMonitorUpdateOperatorMaskedNewHeaderWithoutStoredRejected(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrmasknonew")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health", nil)
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api.example.com/health", "X-New: ****")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (operator, masked new header, no stored) status = %d, want 422: %s", updatePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "X-New") {
		t.Errorf("POST %s (operator, masked new header) error body missing header name: %s", updatePath, body)
	}
	if got := httpHeaderOf(t, s, created.ID, "X-New"); got != "" {
		t.Fatalf("header X-New = %q after rejected update, want not saved at all", got)
	}
}

func TestWebMonitorUpdateOperatorRenamedMaskedHeaderRejected(t *testing.T) {
	s := newMonitorFormStack(t)
	s.uptime.SetKeyring(mustKeyring(t, "test-master-key-A2b"))
	proj, _, memberCookie := ownerAndMember(t, s, "monhdrrename")

	created := createHeaderMonitor(t, s, proj.ID, "https://api.example.com/health",
		map[string]string{"Authorization": "Bearer supersecret"})
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	form := operatorUpdateForm("https://api.example.com/health", "Authz: ****")
	resp := postForm(t, s.srv, updatePath, form, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (operator, renamed masked header) status = %d, want 422: %s", updatePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Authz") {
		t.Errorf("POST %s (operator, renamed masked header) error body missing new header name: %s", updatePath, body)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authorization"); got != "Bearer supersecret" {
		t.Fatalf("header Authorization = %q after rejected rename, want unchanged secret", got)
	}
	if got := httpHeaderOf(t, s, created.ID, "Authz"); got != "" {
		t.Fatalf("header Authz = %q after rejected rename, want not created with literal mask", got)
	}
}
