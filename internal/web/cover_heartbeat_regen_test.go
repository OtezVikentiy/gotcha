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

// TestWebMonitorHeartbeatRegenerate — перевыпуск heartbeat-токена (L10 follow-up):
// owner получает новый URL пинга один раз (200), участник без прав — 404.
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

	// owner: 200, показан новый URL пинга и cron-сниппет.
	resp := postForm(t, s.srv, path, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner regenerate: status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), s.srv.URL+"/uptime/hb/") || !strings.Contains(string(body), "curl") {
		t.Fatalf("owner regenerate: missing ping URL/cron: %s", body)
	}

	// member (без прав управления): 404.
	resp = postForm(t, s.srv, path, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member regenerate: status = %d, want 404", resp.StatusCode)
	}
}
