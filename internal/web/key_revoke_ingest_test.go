package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

const revokeIngestEvent = `{"event_id":"3c1a5d2e9f0b4a6c8d7e1f2a3b4c5d6e","level":"error",` +
	`"exception":{"values":[{"type":"ValueError","value":"revoked key e2e"}]}}`

// KeyCache свежий на каждый вызов: иначе тест мерил бы 30с TTL кеша, а не фильтр revoked_at
// в KeyByPublic.
func newIngestServer(t *testing.T, s *issuesStack) *httptest.Server {
	t.Helper()
	pipeline := ingest.NewPipeline(s.issues, s.batcher)
	pipeline.Start()
	t.Cleanup(func() { _ = pipeline.Close(context.Background()) })
	h := ingest.NewHandler(ingest.NewKeyCache(s.org), ingest.NewOrgQuota(s.org), pipeline, 1<<20)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postStore(t *testing.T, srv *httptest.Server, projectID int64, publicKey string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/%d/store/", srv.URL, projectID), strings.NewReader(revokeIngestEvent))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+publicKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post store: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// KeyByPublic отдаёт только живые ключи (revoked_at IS NULL) — без этого фильтра отозванный
// ключ продолжал бы аутентифицировать приём.
func TestWebKeyRevokeRejectsIngest(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, ownerCookie := registerAndLogin(t, s, "key-revoke-owner@example.com")
	project := createProject(t, s, ownerID, "key-revoke-org", "key-revoke-proj")

	keys, err := s.org.CreateKeys(context.Background(), project.ID, org.KindLegacy)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	key := keys[0]

	if status, body := postStore(t, newIngestServer(t, s), project.ID, key.PublicKey); status != http.StatusOK {
		t.Fatalf("приём живым ключом: status = %d, want 200; body %q", status, body)
	}

	revokePath := fmt.Sprintf("/projects/%d/settings/keys/revoke", project.ID)
	resp := postForm(t, s.srv, revokePath, url.Values{
		"key_id":    {strconv.FormatInt(key.ID, 10)},
		"confirmed": {"yes"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST keys/revoke status = %d, want 303", resp.StatusCode)
	}

	status, body := postStore(t, newIngestServer(t, s), project.ID, key.PublicKey)
	if status != http.StatusForbidden {
		t.Fatalf("приём отозванным ключом: status = %d, want 403; body %q", status, body)
	}
	if !strings.Contains(body, "invalid sentry_key") {
		t.Fatalf("приём отозванным ключом должен отбиваться как неизвестный ключ, body %q", body)
	}
}
