package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

var probeTokenRe = regexp.MustCompile(`<code class="probe-token">([0-9a-f]{64})</code>`)

func extractProbeToken(t *testing.T, body string) string {
	t.Helper()
	m := probeTokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("probe token not found in body: %s", body)
	}
	return m[1]
}

func TestWebProbes(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	adminID, adminCookie := orgSettingsRegister(t, authSvc, "probes-admin@example.com")
	ownerID, _ := orgSettingsRegister(t, authSvc, "probes-owner@example.com")

	o, err := orgSvc.CreateOrg(ctx, "probes-co", "Probes Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}

	probesPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/probes"
	revokePath := probesPath + "/revoke"

	resp := getWithCookie(t, s.srv, probesPath, adminCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (admin) status = %d, want 200: %s", probesPath, resp.StatusCode, body)
	}

	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {"ru-msk"}}, "", adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", probesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {"  "}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty region) status = %d, want 422: %s", probesPath, resp.StatusCode, body)
	}

	resp = postForm(t, s.srv, probesPath, url.Values{"name": {strings.Repeat("x", 41)}, "region": {"ru-msk"}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (long name) status = %d, want 422: %s", probesPath, resp.StatusCode, body)
	}

	// Иначе выносная проба лизила бы задания встроенной.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {uptime.DefaultRegion}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (region=%s) status = %d, want 422: %s", probesPath, uptime.DefaultRegion, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "зарезервирован") {
		t.Fatalf("POST %s (region=%s) must explain the reserved region: %s", probesPath, uptime.DefaultRegion, body)
	}

	if probes, err := s.uptime.Probes(ctx, o.ID); err != nil || len(probes) != 0 {
		t.Fatalf("probes after rejected POSTs = %d, err=%v, want 0", len(probes), err)
	}

	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"Moscow probe"}, "region": {"ru-msk"}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200: %s", probesPath, resp.StatusCode, body)
	}
	token := extractProbeToken(t, string(body))
	if !strings.Contains(string(body), "GOTCHA_PROBE_KEY="+token) {
		t.Fatalf("POST %s missing docker run line with token: %s", probesPath, body)
	}
	if !strings.Contains(string(body), "GOTCHA_PROBE_SERVER_URL="+s.srv.URL) {
		t.Fatalf("POST %s missing docker run line with server url: %s", probesPath, body)
	}
	// Статус локализован — проверяем класс бейджа (badge-danger), а не сырой текст "offline".
	if !strings.Contains(string(body), "badge-danger") {
		t.Fatalf("POST %s: fresh probe must render as offline (badge-danger): %s", probesPath, body)
	}

	p, err := s.uptime.ProbeByToken(ctx, token)
	if err != nil {
		t.Fatalf("ProbeByToken after create: %v", err)
	}
	if p.OrgID != o.ID || p.Region != "ru-msk" || p.Name != "Moscow probe" {
		t.Fatalf("created probe = %+v, want org=%d region=ru-msk name=Moscow probe", p, o.ID)
	}

	resp = getWithCookie(t, s.srv, probesPath, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", probesPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), token) {
		t.Fatalf("GET %s leaks raw probe token: %s", probesPath, body)
	}
	if !strings.Contains(string(body), "Moscow probe") || !strings.Contains(string(body), "ru-msk") {
		t.Fatalf("GET %s missing probe row: %s", probesPath, body)
	}

	confirmResp := postForm(t, s.srv, revokePath, url.Values{"probe_id": {strconv.FormatInt(p.ID, 10)}}, s.srv.URL, adminCookie)
	confirmBody, _ := io.ReadAll(confirmResp.Body)
	confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK || !strings.Contains(string(confirmBody), "Moscow probe") {
		t.Fatalf("подтверждение отзыва пробы не называет её: status=%d, %s", confirmResp.StatusCode, confirmBody)
	}

	// ProbeByToken фильтрует revoked_at IS NULL — отозванная лизить больше не может.
	resp = postForm(t, s.srv, revokePath, url.Values{"confirmed": {"yes"}, "probe_id": {strconv.FormatInt(p.ID, 10)}}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", revokePath, resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != probesPath {
		t.Fatalf("POST %s Location = %q, want %s", revokePath, got, probesPath)
	}
	if _, err := s.uptime.ProbeByToken(ctx, token); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("ProbeByToken after revoke: err = %v, want ErrNotFound", err)
	}
	probes, err := s.uptime.Probes(ctx, o.ID)
	if err != nil || len(probes) != 1 || !probes[0].Revoked {
		t.Fatalf("probes after revoke = %+v, err=%v, want one revoked", probes, err)
	}

	resp = getWithCookie(t, s.srv, probesPath, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "badge-neutral") {
		t.Fatalf("GET %s missing revoked marker (badge-neutral): %s", probesPath, body)
	}
	if strings.Contains(string(body), "probe-revoke-form") {
		t.Fatalf("GET %s: revoked probe must not show a revoke form: %s", probesPath, body)
	}

	resp = postForm(t, s.srv, revokePath, url.Values{"confirmed": {"yes"}, "probe_id": {strconv.FormatInt(p.ID, 10)}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (double revoke) status = %d, want 422: %s", revokePath, resp.StatusCode, body)
	}
}

func TestWebProbesAccess(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "probes-access-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "probes-access-member@example.com")

	o, err := orgSvc.CreateOrg(ctx, "probes-access-co", "Probes Access Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	otherID, _ := orgSettingsRegister(t, authSvc, "probes-access-other@example.com")
	other, err := orgSvc.CreateOrg(ctx, "probes-other-co", "Probes Other Co", otherID)
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	foreign, _, err := s.uptime.CreateProbe(ctx, other.ID, "eu-fra", "Foreign probe")
	if err != nil {
		t.Fatalf("create foreign probe: %v", err)
	}

	probesPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/probes"
	revokePath := probesPath + "/revoke"

	resp := getWithCookie(t, s.srv, probesPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET %s (member) status = %d, want 403", probesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p"}, "region": {"ru-msk"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", probesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, revokePath, url.Values{"confirmed": {"yes"}, "probe_id": {strconv.FormatInt(foreign.ID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", revokePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, revokePath, url.Values{"confirmed": {"yes"}, "probe_id": {strconv.FormatInt(foreign.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (foreign probe) status = %d, want 404", revokePath, resp.StatusCode)
	}
	probes, err := s.uptime.Probes(ctx, other.ID)
	if err != nil || len(probes) != 1 || probes[0].Revoked {
		t.Fatalf("foreign probes = %+v, err=%v, want one alive", probes, err)
	}
}

// Зарезервирован тот регион, который встроенная проба РЕАЛЬНО лизит
// (GOTCHA_UPTIME_LOCAL_REGION), а не литерал "local".
func TestWebProbesReservedRegionFollowsLocalRegion(t *testing.T) {
	const localRegion = "eu-central"

	s := newUptimeStackInRegion(t, localRegion)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	adminID, adminCookie := orgSettingsRegister(t, authSvc, "region-admin@example.com")
	o, err := orgSvc.CreateOrg(ctx, "region-co", "Region Co", adminID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	probesPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/probes"

	regions, err := s.uptime.Regions(ctx, o.ID)
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	if len(regions) != 1 || regions[0] != localRegion {
		t.Fatalf("Regions() = %v, want [%s] — the built-in region must be the one the runner leases", regions, localRegion)
	}

	resp := postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {localRegion}}, s.srv.URL, adminCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (region=%s) status = %d, want 422: %s", probesPath, localRegion, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "зарезервирован") {
		t.Fatalf("POST %s (region=%s) must explain the reserved region: %s", probesPath, localRegion, body)
	}
	if probes, err := s.uptime.Probes(ctx, o.ID); err != nil || len(probes) != 0 {
		t.Fatalf("probes after the rejected POST = %d, err=%v, want 0", len(probes), err)
	}

	// А «local» здесь ничем не занят — обычное имя региона.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {uptime.DefaultRegion}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (region=%s) status = %d, want 200 (not the built-in region here): %s",
			probesPath, uptime.DefaultRegion, resp.StatusCode, body)
	}

	regions, err = s.uptime.Regions(ctx, o.ID)
	if err != nil {
		t.Fatalf("Regions after create: %v", err)
	}
	if len(regions) != 2 || regions[0] != localRegion || regions[1] != uptime.DefaultRegion {
		t.Fatalf("Regions() = %v, want [%s %s]", regions, localRegion, uptime.DefaultRegion)
	}
}
