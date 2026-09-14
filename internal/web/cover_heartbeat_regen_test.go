package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

var heartbeatTokenRe = regexp.MustCompile(`/uptime/hb/([0-9a-f]{64})`)

// Единственная причина ротации — вывести из строя утёкший токен: страница может перерисовать
// URL, не тронув значение в БД, и все прежние ассерты останутся зелёными.
func TestWebMonitorHeartbeatRegenerateInvalidatesOldToken(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, _ := ownerAndMember(t, s, "hbregentoken")

	cfg, _ := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: proj.ID, Name: "cron", Kind: uptime.KindHeartbeat, Enabled: true,
		IntervalSeconds: 3600, TimeoutSeconds: 30, FailThreshold: 1, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusAny, Config: cfg,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}
	oldToken := created.HeartbeatToken
	if oldToken == "" {
		t.Fatalf("HeartbeatToken is empty, want generated token")
	}

	pingOld := s.srv.URL + "/uptime/hb/" + oldToken
	before, err := http.Post(pingOld, "", nil)
	if err != nil {
		t.Fatalf("ping со старым токеном до ротации: %v", err)
	}
	before.Body.Close()
	if before.StatusCode != http.StatusOK {
		t.Fatalf("ping со старым токеном до ротации: статус = %d, want 200", before.StatusCode)
	}

	path := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/heartbeat/regenerate"
	resp := postForm(t, s.srv, path, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("regenerate: status = %d, want 200: %s", resp.StatusCode, body)
	}
	m := heartbeatTokenRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("не нашли новый токен в ответе: %s", body)
	}
	newToken := m[1]
	if newToken == oldToken {
		t.Fatalf("токен не изменился после ротации: %s", newToken)
	}

	stillOld, err := http.Post(pingOld, "", nil)
	if err != nil {
		t.Fatalf("ping со старым токеном после ротации: %v", err)
	}
	stillOld.Body.Close()
	if stillOld.StatusCode != http.StatusNotFound {
		t.Fatalf("ping со старым токеном после ротации: статус = %d, want 404 — старый токен ещё работает", stillOld.StatusCode)
	}

	pingNew, err := http.Post(s.srv.URL+"/uptime/hb/"+newToken, "", nil)
	if err != nil {
		t.Fatalf("ping с новым токеном: %v", err)
	}
	pingNew.Body.Close()
	if pingNew.StatusCode != http.StatusOK {
		t.Fatalf("ping с новым токеном: статус = %d, want 200", pingNew.StatusCode)
	}
}
