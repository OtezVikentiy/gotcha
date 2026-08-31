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
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET %s (member) status = %d, want 403 (№72)", settingsPath, resp.StatusCode)
	}

	renamePath := settingsPath + "/rename"
	keysPath := settingsPath + "/keys"
	revokePath := keysPath + "/revoke"

	// POST rename под member -> 403 (№72)
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"Hacked"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", renamePath, resp.StatusCode)
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

	// Ключей пока нет -> DSN не показан. DSN рендерится только внутри <pre>
	// (см. templates.ProjectSettings), поэтому проверяем именно этот тег, а
	// не "://" — тот встречается и в xmlns иконки-спрайта в <body>.
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "<pre>") {
		t.Fatalf("GET %s unexpectedly has a DSN before any key created: %s", settingsPath, body)
	}

	// POST keys create -> 303, ключ появился. Форма выбора типа появится в
	// Task 4 — здесь шлём kind напрямую, как это будет делать та форма.
	resp = postForm(t, s.srv, keysPath, url.Values{"kind": {"server"}}, s.srv.URL, ownerCookie)
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
	otherKeys, err := orgSvc.CreateKeys(context.Background(), otherProj.ID, org.KindServer)
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	otherKey := otherKeys[0]
	resp = postForm(t, s.srv, revokePath, url.Values{"confirmed": {"yes"}, "key_id": {strconv.FormatInt(otherKey.ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (foreign key) status = %d, want 404: %s", revokePath, resp.StatusCode, body)
	}
	if k2, err := orgSvc.KeysForProject(context.Background(), otherProj.ID); err != nil || k2[0].Revoked {
		t.Fatalf("other project's key revoked unexpectedly: %+v err=%v", k2, err)
	}

	// Отзыв своего ключа (с confirmed=yes — без него revoke только показал бы
	// страницу подтверждения, см. TestWebProjectSettingsRevokeConfirmGate) +
	// выпуск нового -> DSN обновился.
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(firstKeyID, 10)}, "confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", revokePath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, keysPath, url.Values{"kind": {"server"}}, s.srv.URL, ownerCookie)
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
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403 (№72)", perfPath, resp.StatusCode)
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

// TestWebProjectRegressionSettings — секция «Регрессии» (этап 4, план 5,
// задача 2): форма показывает текущие значения (проценты); сохранение пишет
// perf_regression_config, и trace.RegressionConfigFromJSON читает пороги
// обратно (round-trip со значениями ВЫШЕ дефолтных, чтобы опечатка ключа
// завалила тест); recovery ≥ threshold / threshold вне (0,1] / window=0 /
// отрицательный пол / NaN → 422 с сохранением ввода; member → 404 на POST.
func TestWebProjectRegressionSettings(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "regset-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "regset-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "regset-co", "RegSet Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "regset-proj", "RegSet Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	regPath := settingsPath + "/regressions"

	// GET owner: форма «Регрессии» с дефолтами (проценты: 25 = 0.25, 10 = 0.10).
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`name="threshold_pct"`, `name="recovery_pct"`, `name="window_minutes"`,
		`name="min_samples"`, `name="duration_floor_ms"`, `name="floor_lcp"`,
		`name="floor_inp"`, `name="floor_cls"`, `name="floor_fcp"`, `name="floor_ttfb"`,
		`name="enabled"`, `name="seasonal_enabled"`, `name="seasonal_weeks"`,
		`value="25"`, `value="10"`, `value="60"`, `value="100"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s Регрессии form missing %q: %s", settingsPath, want, body)
		}
	}

	// Базовая валидная форма (значения ВЫШЕ дефолтов).
	valid := func() url.Values {
		return url.Values{
			"threshold_pct": {"40"}, "recovery_pct": {"20"},
			"window_minutes": {"90"}, "min_samples": {"150"},
			"duration_floor_ms": {"250"},
			"floor_lcp":         {"300"}, "floor_inp": {"80"}, "floor_cls": {"0.1"},
			"floor_fcp": {"300"}, "floor_ttfb": {"300"}, "enabled": {"1"},
			"seasonal_weeks": {"4"},
		}
	}

	// member (не owner/admin) → 404 на POST.
	resp = postForm(t, s.srv, regPath, valid(), s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403 (№72)", regPath, resp.StatusCode)
	}

	// Валидное сохранение → 303, пороги читаются обратно через RegressionConfigFromJSON.
	resp = postForm(t, s.srv, regPath, valid(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (valid) status = %d, want 303", regPath, resp.StatusCode)
	}
	gotProj, err := orgSvc.GetProject(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	cfg, err := trace.RegressionConfigFromJSON([]byte(gotProj.PerfRegressionConfig))
	if err != nil {
		t.Fatalf("RegressionConfigFromJSON(%q): %v", gotProj.PerfRegressionConfig, err)
	}
	approx := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if !approx(cfg.ThresholdPct, 0.40) || !approx(cfg.RecoveryPct, 0.20) {
		t.Fatalf("round-trip threshold/recovery = %v/%v, want 0.40/0.20", cfg.ThresholdPct, cfg.RecoveryPct)
	}
	if cfg.WindowMinutes != 90 || cfg.MinSamples != 150 {
		t.Fatalf("round-trip window/min_samples = %d/%d, want 90/150", cfg.WindowMinutes, cfg.MinSamples)
	}
	if !approx(cfg.DurationFloorMs, 250) || !approx(cfg.Floor("lcp"), 300) ||
		!approx(cfg.Floor("inp"), 80) || !approx(cfg.Floor("cls"), 0.1) ||
		!approx(cfg.Floor("fcp"), 300) || !approx(cfg.Floor("ttfb"), 300) {
		t.Fatalf("round-trip floors = dur %v lcp %v inp %v cls %v fcp %v ttfb %v",
			cfg.DurationFloorMs, cfg.Floor("lcp"), cfg.Floor("inp"), cfg.Floor("cls"), cfg.Floor("fcp"), cfg.Floor("ttfb"))
	}
	if !cfg.Enabled {
		t.Fatalf("round-trip Enabled = false, want true")
	}

	// Снятый чекбокс enabled → сохраняется как false (присутствие поля = вкл).
	noEnabled := valid()
	noEnabled.Del("enabled")
	resp = postForm(t, s.srv, regPath, noEnabled, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (no enabled) status = %d, want 303", regPath, resp.StatusCode)
	}
	gotProj, _ = orgSvc.GetProject(context.Background(), proj.ID)
	cfg, _ = trace.RegressionConfigFromJSON([]byte(gotProj.PerfRegressionConfig))
	if cfg.Enabled {
		t.Fatalf("Enabled after unchecked = true, want false")
	}
	// Сезонный режим: seasonal_enabled=on + seasonal_weeks=6 → сохраняется в конфиг.
	seasonal := valid()
	seasonal.Set("seasonal_enabled", "1")
	seasonal.Set("seasonal_weeks", "6")
	resp = postForm(t, s.srv, regPath, seasonal, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (seasonal) status = %d, want 303", regPath, resp.StatusCode)
	}
	gotProj, _ = orgSvc.GetProject(context.Background(), proj.ID)
	cfg, _ = trace.RegressionConfigFromJSON([]byte(gotProj.PerfRegressionConfig))
	if !cfg.SeasonalEnabled || cfg.SeasonalWeeks != 6 {
		t.Fatalf("round-trip seasonal = %v/%d, want true/6", cfg.SeasonalEnabled, cfg.SeasonalWeeks)
	}
	// Снятый чекбокс seasonal_enabled → false (присутствие поля = вкл), недели хранятся.
	noSeasonal := valid()
	noSeasonal.Set("seasonal_weeks", "5")
	resp = postForm(t, s.srv, regPath, noSeasonal, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	gotProj, _ = orgSvc.GetProject(context.Background(), proj.ID)
	cfg, _ = trace.RegressionConfigFromJSON([]byte(gotProj.PerfRegressionConfig))
	if cfg.SeasonalEnabled || cfg.SeasonalWeeks != 5 {
		t.Fatalf("seasonal off round-trip = %v/%d, want false/5", cfg.SeasonalEnabled, cfg.SeasonalWeeks)
	}

	// Вернём валидную запись с enabled для дальнейшей проверки «не менялось».
	resp = postForm(t, s.srv, regPath, valid(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Форма после сохранения показывает сохранённые значения (проценты 40/20).
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `value="40"`) || !strings.Contains(string(body), `value="20"`) {
		t.Fatalf("GET %s missing saved regression values: %s", settingsPath, body)
	}

	// Невалидные входы → 422 с сохранением отправленного значения offending-поля.
	bad := []struct {
		name  string
		field string
		val   string
	}{
		{"recovery>=threshold", "recovery_pct", "40"}, // threshold=40 → recovery не меньше
		{"threshold=0", "threshold_pct", "0"},
		{"threshold>100", "threshold_pct", "150"},
		{"window=0", "window_minutes", "0"},
		{"min_samples=0", "min_samples", "0"},
		{"negative floor", "floor_lcp", "-5"},
		{"NaN duration", "duration_floor_ms", "NaN"},
		{"seasonal_weeks=1", "seasonal_weeks", "1"},
		{"seasonal_weeks=99", "seasonal_weeks", "99"},
		{"seasonal_weeks empty", "seasonal_weeks", ""},
	}
	for _, tc := range bad {
		form := valid()
		form.Set(tc.field, tc.val)
		resp = postForm(t, s.srv, regPath, form, s.srv.URL, ownerCookie)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %s (%s) status = %d, want 422: %s", regPath, tc.name, resp.StatusCode, body)
		}
		if want := `value="` + tc.val + `"`; !strings.Contains(string(body), want) {
			t.Fatalf("POST %s (%s) 422 form did not preserve %q: %s", regPath, tc.name, want, body)
		}
	}

	// БД по-прежнему держит последнюю валидную запись (threshold 0.40).
	gotProj, err = orgSvc.GetProject(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("GetProject after bad posts: %v", err)
	}
	cfg, _ = trace.RegressionConfigFromJSON([]byte(gotProj.PerfRegressionConfig))
	if !approx(cfg.ThresholdPct, 0.40) {
		t.Fatalf("ThresholdPct after 422s = %v, want unchanged 0.40", cfg.ThresholdPct)
	}
}

// TestProjectSettingsKeyCreateRequiresKind — форма без выбранного типа не
// создаёт ключ: 422 и сообщение, а не молчаливый ключ с произвольным типом.
func TestProjectSettingsKeyCreateRequiresKind(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "keykind-req-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "keykind-req-co", "KeyKindReq Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "keykind-req-proj", "KeyKindReq Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keysPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings/keys"

	resp := postForm(t, s.srv, keysPath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (no kind) status = %d, want 422: %s", keysPath, resp.StatusCode, body)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("KeysForProject after kind-less POST = %+v, err=%v, want none", keys, err)
	}
}

// TestProjectSettingsKeyCreateRejectsLegacy — legacy через UI не выпускается:
// это тип ключей, выпущенных ДО появления типов.
func TestProjectSettingsKeyCreateRejectsLegacy(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "keykind-legacy-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "keykind-legacy-co", "KeyKindLegacy Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "keykind-legacy-proj", "KeyKindLegacy Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keysPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings/keys"

	resp := postForm(t, s.srv, keysPath, url.Values{"kind": {"legacy"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (kind=legacy) status = %d, want 422: %s", keysPath, resp.StatusCode, body)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("KeysForProject after kind=legacy POST = %+v, err=%v, want none", keys, err)
	}
}

// TestProjectSettingsKeyCreateKind — выбранный тип доезжает до БД.
func TestProjectSettingsKeyCreateKind(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "keykind-agent-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "keykind-agent-co", "KeyKindAgent Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "keykind-agent-proj", "KeyKindAgent Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keysPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings/keys"

	resp := postForm(t, s.srv, keysPath, url.Values{"kind": {"agent"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (kind=agent) status = %d, want 303", keysPath, resp.StatusCode)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("KeysForProject after kind=agent POST = %+v, err=%v, want 1 key", keys, err)
	}
	if keys[0].Kind != org.KindAgent {
		t.Fatalf("created key Kind = %q, want %q", keys[0].Kind, org.KindAgent)
	}
}

// TestProjectSettingsPageShowsKindsAndDSN — таблица показывает тип каждого
// ключа и его СОБСТВЕННЫЙ DSN; отдельного «DSN проекта» на странице нет.
func TestProjectSettingsPageShowsKindsAndDSN(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "keykind-show-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "keykind-show-co", "KeyKindShow Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "keykind-show-proj", "KeyKindShow Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"

	newKeys, err := orgSvc.CreateKeys(context.Background(), proj.ID, org.KindBrowser, org.KindAgent)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	var browserKey, agentKey org.Key
	for _, k := range newKeys {
		switch k.Kind {
		case org.KindBrowser:
			browserKey = k
		case org.KindAgent:
			agentKey = k
		}
	}
	if browserKey.ID == 0 || agentKey.ID == 0 {
		t.Fatalf("CreateKeys did not return both kinds: %+v", newKeys)
	}
	// legacy-ключ через UI не выпускается (шаг блокируется обработчиком), но
	// на уровне сервиса kind.Valid() пропускает legacy — им и пользовались
	// ключи, выпущенные ДО появления типов. Заводим такой напрямую.
	legacyKeys, err := orgSvc.CreateKeys(context.Background(), proj.ID, org.KindLegacy)
	if err != nil {
		t.Fatalf("create legacy key: %v", err)
	}
	legacyKey := legacyKeys[0]

	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	html := string(body)

	browserDSN := "://" + browserKey.PublicKey + "@"
	if !strings.Contains(html, browserDSN) {
		t.Fatalf("GET %s missing browser key DSN %q: %s", settingsPath, browserDSN, html)
	}
	agentDSN := "://" + agentKey.PublicKey + "@"
	if !strings.Contains(html, agentDSN) {
		t.Fatalf("GET %s missing agent key DSN %q: %s", settingsPath, agentDSN, html)
	}
	legacyDSN := "://" + legacyKey.PublicKey + "@"
	if !strings.Contains(html, legacyDSN) {
		t.Fatalf("GET %s missing legacy key DSN %q: %s", settingsPath, legacyDSN, html)
	}
	if !strings.Contains(html, "Браузер") && !strings.Contains(html, "Browser") {
		t.Fatalf("GET %s missing browser kind label: %s", settingsPath, html)
	}
	if !strings.Contains(html, "Агент") && !strings.Contains(html, "Agent") {
		t.Fatalf("GET %s missing agent kind label: %s", settingsPath, html)
	}
	if !strings.Contains(html, "/docs/keys") {
		t.Fatalf("GET %s missing legacy hint link to /docs/keys: %s", settingsPath, html)
	}
}

// TestProjectSettingsRevokeLastOfKindWarns — подтверждение отзыва последнего
// ЖИВОГО ключа своего типа предупреждает, что приём этого класса телеметрии
// остановится; при наличии второго живого ключа того же типа — обычный
// текст.
func TestProjectSettingsRevokeLastOfKindWarns(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "keykind-warn-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "keykind-warn-co", "KeyKindWarn Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "keykind-warn-proj", "KeyKindWarn Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	revokePath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings/keys/revoke"

	// Единственный живой ключ типа agent -> предупреждение.
	soleKeys, err := orgSvc.CreateKeys(context.Background(), proj.ID, org.KindAgent)
	if err != nil {
		t.Fatalf("create sole agent key: %v", err)
	}
	soleKey := soleKeys[0]
	resp := postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(soleKey.ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (sole agent key) status = %d, want 200: %s", revokePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "confirm.key_revoke.last_of_kind.message") &&
		!strings.Contains(string(body), "последний активный ключ") {
		t.Fatalf("POST %s (sole agent key) missing last-of-kind warning: %s", revokePath, body)
	}

	// Второй живой ключ того же типа -> обычный текст, без предупреждения.
	pairKeys, err := orgSvc.CreateKeys(context.Background(), proj.ID, org.KindServer, org.KindServer)
	if err != nil {
		t.Fatalf("create two server keys: %v", err)
	}
	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(pairKeys[0].ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (paired server key) status = %d, want 200: %s", revokePath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "confirm.key_revoke.last_of_kind.message") ||
		strings.Contains(string(body), "последний активный ключ") {
		t.Fatalf("POST %s (paired server key) unexpectedly shows last-of-kind warning: %s", revokePath, body)
	}
}
