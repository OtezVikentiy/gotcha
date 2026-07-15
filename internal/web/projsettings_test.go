package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestWebProjectSettings — сквозной сценарий задачи 3 (настройки проекта):
// owner видит настройки, member — 404, rename работает и пустое имя → 422,
// создание/отзыв DSN-ключа, отзыв ЧУЖОГО key_id → 404, DSN обновляется после
// revoke+create.
func TestWebProjectSettings(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "projsettings-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "projsettings-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "projsettings-co", "ProjSettings Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "projsettings-proj", "ProjSettings Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"

	// GET owner -> 200, имя и платформа видны.
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "ProjSettings Proj") || !strings.Contains(string(body), "go") {
		t.Fatalf("GET %s missing project name/platform: %s", settingsPath, body)
	}

	// GET member (не owner/admin) -> 404
	resp = getWithCookie(t, s.srv, settingsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", settingsPath, resp.StatusCode)
	}

	renamePath := settingsPath + "/rename"
	keysPath := settingsPath + "/keys"
	revokePath := keysPath + "/revoke"

	// POST rename под member -> 404
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"Hacked"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", renamePath, resp.StatusCode)
	}

	// POST rename без Origin -> 403
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"New Name"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", renamePath, resp.StatusCode)
	}

	// POST rename валидный -> 303
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"New Name"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", renamePath, resp.StatusCode)
	}

	// POST rename пустое имя -> 422
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {""}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty name) status = %d, want 422: %s", renamePath, resp.StatusCode, body)
	}

	// Ключей пока нет -> DSN не показан.
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "://") {
		t.Fatalf("GET %s unexpectedly has a DSN before any key created: %s", settingsPath, body)
	}

	// POST keys create -> 303, ключ появился.
	resp = postForm(t, s.srv, keysPath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", keysPath, resp.StatusCode)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("KeysForProject after create = %+v, err=%v, want 1 key", keys, err)
	}
	firstKeyID := keys[0].ID
	firstDSN := "://" + keys[0].PublicKey + "@"

	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), firstDSN) {
		t.Fatalf("GET %s missing DSN %q: %s", settingsPath, firstDSN, body)
	}

	// Отзыв ЧУЖОГО key_id (принадлежащего другому проекту) -> 404, ключ не тронут.
	otherProj, err := orgSvc.CreateProject(context.Background(), o.ID, "projsettings-other", "Other Proj", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	otherKey, err := orgSvc.CreateKey(context.Background(), otherProj.ID)
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(otherKey.ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (foreign key) status = %d, want 404: %s", revokePath, resp.StatusCode, body)
	}
	if k2, err := orgSvc.KeysForProject(context.Background(), otherProj.ID); err != nil || k2[0].Revoked {
		t.Fatalf("other project's key revoked unexpectedly: %+v err=%v", k2, err)
	}

	// Отзыв своего ключа + выпуск нового -> DSN обновился.
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(firstKeyID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", revokePath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, keysPath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (second key) status = %d, want 303", keysPath, resp.StatusCode)
	}
	keys, err = orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("KeysForProject after revoke+create = %+v, err=%v, want 2 keys", keys, err)
	}
	var newLiveKey org.Key
	for _, k := range keys {
		if !k.Revoked {
			newLiveKey = k
		}
	}
	if newLiveKey.ID == 0 || newLiveKey.ID == firstKeyID {
		t.Fatalf("no new live key found: %+v", keys)
	}
	newDSN := "://" + newLiveKey.PublicKey + "@"
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), newDSN) {
		t.Fatalf("GET %s missing updated DSN %q: %s", settingsPath, newDSN, body)
	}
	if strings.Contains(string(body), firstDSN) {
		t.Fatalf("GET %s still shows old (revoked) DSN %q: %s", settingsPath, firstDSN, body)
	}
}

// TestWebProjectPerformanceSettings — секция «Performance» (этап 3, план 5,
// задача 2): форма показывает текущие значения; сохранение пишет
// sample_rate/apdex/detector_config в БД, и trace.ConfigFromJSON читает пороги
// обратно (round-trip); невалидные sample_rate/apdex/пороги → 422 с
// сохранением ввода; member → 404 на POST.
func TestWebProjectPerformanceSettings(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "perfset-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "perfset-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "perfset-co", "PerfSet Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "perfset-proj", "PerfSet Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	perfPath := settingsPath + "/performance"

	// GET owner: форма Performance с дефолтными порогами детекторов.
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	def := trace.DefaultDetectorConfig()
	for _, want := range []string{
		`name="sample_rate"`, `name="apdex_threshold_ms"`,
		`name="n_plus_one_min"`, `name="n_plus_one_min_total_ms"`,
		`name="slow_db_ms"`, `name="http_flood_min"`,
		`value="` + strconv.Itoa(def.SlowDBMs) + `"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s Performance form missing %q: %s", settingsPath, want, body)
		}
	}

	// member (не owner/admin) → 404 на POST.
	resp = postForm(t, s.srv, perfPath, url.Values{
		"sample_rate": {"0.5"}, "apdex_threshold_ms": {"300"},
		"n_plus_one_min": {"5"}, "n_plus_one_min_total_ms": {"20"},
		"slow_db_ms": {"500"}, "http_flood_min": {"10"},
	}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", perfPath, resp.StatusCode)
	}

	// Валидное сохранение → 303, значения в БД, пороги читаются обратно.
	resp = postForm(t, s.srv, perfPath, url.Values{
		"sample_rate": {"0.25"}, "apdex_threshold_ms": {"450"},
		"n_plus_one_min": {"7"}, "n_plus_one_min_total_ms": {"30"},
		"slow_db_ms": {"250"}, "http_flood_min": {"15"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (valid) status = %d, want 303", perfPath, resp.StatusCode)
	}
	got, err := orgSvc.GetProject(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.TransactionSampleRate != 0.25 || got.ApdexThresholdMS != 450 {
		t.Fatalf("saved sample_rate/apdex = %v/%d, want 0.25/450", got.TransactionSampleRate, got.ApdexThresholdMS)
	}
	cfg, err := trace.ConfigFromJSON([]byte(got.PerfDetectorConfig))
	if err != nil {
		t.Fatalf("ConfigFromJSON(%q): %v", got.PerfDetectorConfig, err)
	}
	if cfg.NPlusOneMin != 7 || cfg.NPlusOneMinTotalMs != 30 || cfg.SlowDBMs != 250 || cfg.HTTPFloodMin != 15 {
		t.Fatalf("round-trip cfg = %+v, want {7 30 250 15}", cfg)
	}

	// Форма после сохранения показывает сохранённые значения.
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `value="250"`) || !strings.Contains(string(body), `value="0.25"`) {
		t.Fatalf("GET %s missing saved perf values: %s", settingsPath, body)
	}

	// Невалидные входы → 422; сохранённое в БД не меняется.
	bad := []struct {
		name string
		form url.Values
	}{
		{"sample_rate=1.5", url.Values{"sample_rate": {"1.5"}, "apdex_threshold_ms": {"450"}, "n_plus_one_min": {"7"}, "n_plus_one_min_total_ms": {"30"}, "slow_db_ms": {"250"}, "http_flood_min": {"15"}}},
		{"apdex_threshold_ms=0", url.Values{"sample_rate": {"0.25"}, "apdex_threshold_ms": {"0"}, "n_plus_one_min": {"7"}, "n_plus_one_min_total_ms": {"30"}, "slow_db_ms": {"250"}, "http_flood_min": {"15"}}},
		{"slow_db_ms=0", url.Values{"sample_rate": {"0.25"}, "apdex_threshold_ms": {"450"}, "n_plus_one_min": {"7"}, "n_plus_one_min_total_ms": {"30"}, "slow_db_ms": {"0"}, "http_flood_min": {"15"}}},
		{"sample_rate=NaN", url.Values{"sample_rate": {"NaN"}, "apdex_threshold_ms": {"450"}, "n_plus_one_min": {"7"}, "n_plus_one_min_total_ms": {"30"}, "slow_db_ms": {"250"}, "http_flood_min": {"15"}}},
	}
	for _, tc := range bad {
		resp = postForm(t, s.srv, perfPath, tc.form, s.srv.URL, ownerCookie)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %s (%s) status = %d, want 422: %s", perfPath, tc.name, resp.StatusCode, body)
		}
		// Отправленное (невалидное) значение возвращается в форму.
		if want := tc.form.Get(strings.SplitN(tc.name, "=", 2)[0]); !strings.Contains(string(body), `value="`+want+`"`) {
			t.Fatalf("POST %s (%s) 422 form did not preserve submitted %q: %s", perfPath, tc.name, want, body)
		}
	}
	// БД по-прежнему держит валидные значения (последняя удачная запись).
	got, err = orgSvc.GetProject(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("GetProject after bad posts: %v", err)
	}
	if got.ApdexThresholdMS != 450 {
		t.Fatalf("ApdexThresholdMS after 422s = %d, want unchanged 450", got.ApdexThresholdMS)
	}
}
