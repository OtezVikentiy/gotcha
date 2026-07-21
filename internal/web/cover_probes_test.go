package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestCoverProbesValidation — недокрытые ветки orgProbesCreate/Revoke: sameOrigin
// 403; пустое имя → 422; зарезервированный (локальный) регион → 422; revoke
// нечисловой probe_id → 400; чужая проба → 404; повторный отзыв → 422.
func TestCoverProbesValidation(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "cover-probe-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "cover-probe-co", "Cover Probe", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/probes"

	// Create без Origin → 403.
	resp := postForm(t, s.srv, base, url.Values{"name": {"p"}, "region": {"eu"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST probes (no origin) = %d, want 403", resp.StatusCode)
	}

	// Пустое имя → 422.
	resp = postForm(t, s.srv, base, url.Values{"name": {""}, "region": {"eu-west"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST probes (empty name) = %d, want 422", resp.StatusCode)
	}

	// Зарезервированный регион (локальный) → 422.
	resp = postForm(t, s.srv, base, url.Values{"name": {"probe-1"}, "region": {uptime.DefaultRegion}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST probes (reserved region) = %d, want 422", resp.StatusCode)
	}

	// Валидное создание → 200 (страница с одноразовым токеном).
	resp = postForm(t, s.srv, base, url.Values{"name": {"probe-eu"}, "region": {"eu-west"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST probes (valid) = %d, want 200", resp.StatusCode)
	}

	// Revoke нечисловой probe_id → 400.
	resp = postForm(t, s.srv, base+"/revoke", url.Values{"probe_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST probes/revoke (bad probe_id) = %d, want 400", resp.StatusCode)
	}

	// Чужая проба (другой орг) → 404.
	other, err := orgSvc.CreateOrg(ctx, "cover-probe-other", "Other", ownerID)
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	foreign, _, err := s.uptime.CreateProbe(ctx, other.ID, "eu-east", "foreign-probe")
	if err != nil {
		t.Fatalf("create foreign probe: %v", err)
	}
	resp = postForm(t, s.srv, base+"/revoke", url.Values{"probe_id": {strconv.FormatInt(foreign.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST probes/revoke (foreign) = %d, want 404", resp.StatusCode)
	}

	// Повторный отзыв уже отозванной пробы → 422.
	mine, _, err := s.uptime.CreateProbe(ctx, o.ID, "eu-north", "mine-probe")
	if err != nil {
		t.Fatalf("create my probe: %v", err)
	}
	if err := s.uptime.RevokeProbe(ctx, mine.ID); err != nil {
		t.Fatalf("revoke my probe: %v", err)
	}
	resp = postForm(t, s.srv, base+"/revoke", url.Values{"probe_id": {strconv.FormatInt(mine.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST probes/revoke (already revoked) = %d, want 422", resp.StatusCode)
	}
}
