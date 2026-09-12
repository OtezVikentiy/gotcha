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

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestWebMonitorHeartbeatRegenerate(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, memberCookie := ownerAndMember(t, s, "hbregen")

	cfg, _ := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: proj.ID, Name: "cron", Kind: uptime.KindHeartbeat, Enabled: true,
		IntervalSeconds: 3600, TimeoutSeconds: 30, FailThreshold: 1, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusAny, Config: cfg,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}
	path := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/heartbeat/regenerate"

	resp := postForm(t, s.srv, path, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner regenerate (confirm): status = %d, want 200: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), s.srv.URL+"/uptime/hb/") {
		t.Fatalf("токен перевыпущен без подтверждения — рабочий cron ломается одним кликом")
	}
	if !strings.Contains(string(body), `name="confirmed"`) {
		t.Fatalf("страница подтверждения без поля confirmed: %s", body)
	}

	editLink := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/edit"

	resp = postForm(t, s.srv, path, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner regenerate: status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), s.srv.URL+"/uptime/hb/") || !strings.Contains(string(body), "curl") {
		t.Fatalf("owner regenerate: missing ping URL/cron: %s", body)
	}
	if !strings.Contains(string(body), editLink) {
		t.Fatalf("owner regenerate: missing Edit link %s", editLink)
	}

	resp = postForm(t, s.srv, path, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member regenerate: status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), s.srv.URL+"/uptime/hb/") || !strings.Contains(string(body), "curl") {
		t.Fatalf("member regenerate: missing ping URL/cron: %s", body)
	}
	if !strings.Contains(string(body), editLink) {
		t.Fatalf("member regenerate: missing Edit link %s (Task 2 makes Edit an operator action)", editLink)
	}

	resp = getWithCookie(t, s.srv, editLink, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET %s: status = %d, want 200: %s", editLink, resp.StatusCode, body)
	}
}
