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

// probeTokenRe вытаскивает сырой токен пробы из одноразового блока «скопируйте
// сейчас» (уникальный класс probe-token) — тот же приём, что и
// extractInviteLink в orgsettings_test.go.
var probeTokenRe = regexp.MustCompile(`<code class="probe-token">([0-9a-f]{64})</code>`)

func extractProbeToken(t *testing.T, body string) string {
	t.Helper()
	m := probeTokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("probe token not found in body: %s", body)
	}
	return m[1]
}

// TestWebProbes — сквозной сценарий задачи 3 (план 5): admin создаёт пробу,
// сырой токен показан ровно один раз (в теле POST-ответа) и больше нигде,
// проба без last_seen_at показана как offline, revoke отзывает пробу и её
// токен перестаёт аутентифицировать (ProbeByToken → ErrNotFound).
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

	// GET пустой страницы admin'ом → 200.
	resp := getWithCookie(t, s.srv, probesPath, adminCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (admin) status = %d, want 200: %s", probesPath, resp.StatusCode, body)
	}

	// POST без Origin → 403.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {"ru-msk"}}, "", adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", probesPath, resp.StatusCode)
	}

	// POST с пустым регионом → 422, проба не создана.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p1"}, "region": {"  "}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty region) status = %d, want 422: %s", probesPath, resp.StatusCode, body)
	}

	// POST со слишком длинным именем (>40 символов) → 422.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {strings.Repeat("x", 41)}, "region": {"ru-msk"}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (long name) status = %d, want 422: %s", probesPath, resp.StatusCode, body)
	}

	// POST с зарезервированным регионом «local» (uptime.DefaultRegion — регион
	// встроенной пробы центра) → 422 с внятным сообщением, проба не создана:
	// иначе выносная проба лизила бы задания встроенной.
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

	// POST валидный → 200 с сырым токеном и готовой строкой запуска.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"Moscow probe"}, "region": {"ru-msk"}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200: %s", probesPath, resp.StatusCode, body)
	}
	token := extractProbeToken(t, string(body))
	if !strings.Contains(string(body), "GOTCHA_PROBE_TOKEN="+token) {
		t.Fatalf("POST %s missing docker run line with token: %s", probesPath, body)
	}
	if !strings.Contains(string(body), "GOTCHA_SERVER_URL="+s.srv.URL) {
		t.Fatalf("POST %s missing docker run line with server url: %s", probesPath, body)
	}
	// Проба без last_seen_at — offline.
	if !strings.Contains(string(body), "offline") {
		t.Fatalf("POST %s: fresh probe must render as offline: %s", probesPath, body)
	}

	// Токен есть в БД (в виде хеша): ProbeByToken его находит.
	p, err := s.uptime.ProbeByToken(ctx, token)
	if err != nil {
		t.Fatalf("ProbeByToken after create: %v", err)
	}
	if p.OrgID != o.ID || p.Region != "ru-msk" || p.Name != "Moscow probe" {
		t.Fatalf("created probe = %+v, want org=%d region=ru-msk name=Moscow probe", p, o.ID)
	}

	// Повторный GET страницы токен НЕ содержит (показывается ровно один раз).
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

	// Revoke: проба помечена отозванной и перестаёт аутентифицироваться
	// (ProbeByToken фильтрует revoked_at IS NULL — лизить она больше не может).
	resp = postForm(t, s.srv, revokePath, url.Values{"probe_id": {strconv.FormatInt(p.ID, 10)}}, s.srv.URL, adminCookie)
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

	// Отозванная проба показана как revoked, кнопки Revoke у неё больше нет.
	resp = getWithCookie(t, s.srv, probesPath, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "revoked") {
		t.Fatalf("GET %s missing revoked marker: %s", probesPath, body)
	}

	// Повторный revoke той же пробы → 422 (ErrNotFound из RevokeProbe).
	resp = postForm(t, s.srv, revokePath, url.Values{"probe_id": {strconv.FormatInt(p.ID, 10)}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (double revoke) status = %d, want 422: %s", revokePath, resp.StatusCode, body)
	}
}

// TestWebProbesAccess — member организации не видит страницу проб и не может
// ничего на ней сделать (404, не 403 — не палим существование организации), а
// пробу чужой организации нельзя отозвать по id (404).
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

	// Чужая организация со своей пробой.
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

	// member: GET → 404.
	resp := getWithCookie(t, s.srv, probesPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", probesPath, resp.StatusCode)
	}

	// member: POST создания → 404.
	resp = postForm(t, s.srv, probesPath, url.Values{"name": {"p"}, "region": {"ru-msk"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", probesPath, resp.StatusCode)
	}

	// member: POST revoke → 404.
	resp = postForm(t, s.srv, revokePath, url.Values{"probe_id": {strconv.FormatInt(foreign.ID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", revokePath, resp.StatusCode)
	}

	// owner своей организации пытается отозвать пробу ЧУЖОЙ организации → 404,
	// проба не отозвана.
	resp = postForm(t, s.srv, revokePath, url.Values{"probe_id": {strconv.FormatInt(foreign.ID, 10)}}, s.srv.URL, ownerCookie)
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

// TestWebProbesReservedRegionFollowsLocalRegion — зарезервирован тот регион,
// который встроенная проба РЕАЛЬНО лизит (GOTCHA_LOCAL_REGION), а не литерал
// "local". При GOTCHA_LOCAL_REGION=eu-central:
//   - выносную пробу в регионе eu-central завести нельзя (иначе её задания
//     забирал бы org-agnostic LeaseLocal центра, и монитор проверялся бы из
//     центра, молча выдавая себя за eu-central);
//   - форма монитора предлагает регион eu-central, а НЕ "local" — монитор в
//     регионе, который никто не лизит, не проверялся бы никогда;
//   - сам "local" при этом становится обычным именем региона, и пробу в нём
//     завести можно.
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

	// Форма монитора предлагает встроенный регион под его настоящим именем.
	regions, err := s.uptime.Regions(ctx, o.ID)
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	if len(regions) != 1 || regions[0] != localRegion {
		t.Fatalf("Regions() = %v, want [%s] — the built-in region must be the one the runner leases", regions, localRegion)
	}

	// Проба в регионе встроенной пробы → 422, проба не создана.
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
