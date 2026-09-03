package main

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "all" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "all")
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.PostgresDSN != "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable" {
		t.Errorf("PostgresDSN = %q", cfg.PostgresDSN)
	}
	if cfg.ClickHouseDSN != "clickhouse://localhost:9000/gotcha" {
		t.Errorf("ClickHouseDSN = %q", cfg.ClickHouseDSN)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90", cfg.RetentionDays)
	}
	if cfg.SpanRetentionDays != 30 {
		t.Errorf("SpanRetentionDays = %d, want 30", cfg.SpanRetentionDays)
	}
	if cfg.LogRetentionDays != 14 {
		t.Errorf("LogRetentionDays = %d, want 14", cfg.LogRetentionDays)
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota = %d, want 0 (oss unlimited)", cfg.DefaultEventQuota)
	}
	if cfg.MaxEventBytes != 1048576 {
		t.Errorf("MaxEventBytes = %d, want 1048576", cfg.MaxEventBytes)
	}
	if cfg.SecretKey != "insecure-dev-secret" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
	if cfg.UptimeConcurrency != 50 {
		t.Errorf("UptimeConcurrency = %d, want 50", cfg.UptimeConcurrency)
	}
	if cfg.LocalRegion != "local" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "local")
	}
	if cfg.ProbeToken != "" {
		t.Errorf("ProbeToken = %q, want empty", cfg.ProbeToken)
	}
	if cfg.ServerURL != "" {
		t.Errorf("ServerURL = %q, want empty", cfg.ServerURL)
	}
	// rem-A ops-H1: дефолт совпадает с ENV Dockerfile — .env.example с пустым
	// или отсутствующим значением (env_file compose) не должен гасить раздачу.
	if cfg.AgentDistDir != "/opt/gotcha/agent-dist" {
		t.Errorf("AgentDistDir = %q, want /opt/gotcha/agent-dist", cfg.AgentDistDir)
	}
	// rem-A ops-H4: щедрее умолчания New() (10/мин) — установка/обновление
	// парка за одним IP не должны сериализоваться дефолтом, рассчитанным на
	// одиночный сервер.
	if cfg.AgentDistRatePerMin != 120 {
		t.Errorf("AgentDistRatePerMin = %d, want 120", cfg.AgentDistRatePerMin)
	}
	if cfg.ExportDir != "/var/lib/gotcha/exports" {
		t.Errorf("ExportDir = %q, want /var/lib/gotcha/exports", cfg.ExportDir)
	}
	if cfg.ExportTTLHours != 168 {
		t.Errorf("ExportTTLHours = %d, want 168", cfg.ExportTTLHours)
	}
	if cfg.ExportMaxRows != 200_000 {
		t.Errorf("ExportMaxRows = %d, want 200000", cfg.ExportMaxRows)
	}
	if cfg.ExportMaxBytes != 268_435_456 {
		t.Errorf("ExportMaxBytes = %d, want 268435456", cfg.ExportMaxBytes)
	}
	if cfg.ExportDiskBudgetBytes != 5_368_709_120 {
		t.Errorf("ExportDiskBudgetBytes = %d, want 5368709120", cfg.ExportDiskBudgetBytes)
	}
}

// TestLoadConfig_ExportOverrides — все пять переменных выгрузок читаются
// обычными str/intNum/num, каждая независимо переопределяется своим env.
func TestLoadConfig_ExportOverrides(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EXPORT_DIR":               "/data/exports",
		"GOTCHA_EXPORT_RETENTION_HOURS":   "24",
		"GOTCHA_EXPORT_MAX_ROWS":          "1000",
		"GOTCHA_EXPORT_MAX_BYTES":         "1048576",
		"GOTCHA_EXPORT_DISK_BUDGET_BYTES": "2097152",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ExportDir != "/data/exports" {
		t.Errorf("ExportDir = %q, want /data/exports", cfg.ExportDir)
	}
	if cfg.ExportTTLHours != 24 {
		t.Errorf("ExportTTLHours = %d, want 24", cfg.ExportTTLHours)
	}
	if cfg.ExportMaxRows != 1000 {
		t.Errorf("ExportMaxRows = %d, want 1000", cfg.ExportMaxRows)
	}
	if cfg.ExportMaxBytes != 1048576 {
		t.Errorf("ExportMaxBytes = %d, want 1048576", cfg.ExportMaxBytes)
	}
	if cfg.ExportDiskBudgetBytes != 2097152 {
		t.Errorf("ExportDiskBudgetBytes = %d, want 2097152", cfg.ExportDiskBudgetBytes)
	}
}

// TestLoadConfig_AgentDistDirEmptyEnvFallsBackToDefault — rem-A ops-H1: явно
// заданная пустая строка (как её отдаёт docker-compose env_file на
// GOTCHA_DIST_DIR= из .env.example) неотличима для str() от «не
// задано» — не должна гасить раздачу пустым значением поверх ENV образа.
func TestLoadConfig_AgentDistDirEmptyEnvFallsBackToDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DIST_DIR": ""}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentDistDir != "/opt/gotcha/agent-dist" {
		t.Errorf("AgentDistDir = %q, want /opt/gotcha/agent-dist (empty env should fall back to default)", cfg.AgentDistDir)
	}
}

// TestLoadConfig_AgentDistRatePerMinOverride — GOTCHA_DIST_RATE_PER_MIN
// читается как обычный intNum (rem-A ops-H4).
func TestLoadConfig_AgentDistRatePerMinOverride(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DIST_RATE_PER_MIN": "500"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentDistRatePerMin != 500 {
		t.Errorf("AgentDistRatePerMin = %d, want 500", cfg.AgentDistRatePerMin)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := map[string]string{
		"GOTCHA_LISTEN_ADDR":          ":9090",
		"GOTCHA_BASE_URL":             "https://errors.example.com",
		"GOTCHA_PG_DSN":               "postgres://u:p@pg:5432/g",
		"GOTCHA_CH_DSN":               "clickhouse://ch:9000/g",
		"GOTCHA_SMTP_HOST":            "smtp.example.com",
		"GOTCHA_SMTP_PORT":            "465",
		"GOTCHA_SMTP_USER":            "mailer",
		"GOTCHA_SMTP_PASSWORD":        "s3cret",
		"GOTCHA_SMTP_FROM":            "gotcha@example.com",
		"GOTCHA_EVENT_RETENTION_DAYS": "30",
		"GOTCHA_SPAN_RETENTION_DAYS":  "7",
		"GOTCHA_DEFAULT_EVENT_QUOTA":  "50000",
		"GOTCHA_MAX_EVENT_BYTES":      "2097152",
		"GOTCHA_SECRET_KEY":           "prod-secret-at-least-32-bytes-long!",
		"GOTCHA_UPTIME_CONCURRENCY":   "10",
		"GOTCHA_UPTIME_LOCAL_REGION":  "eu-fra",
		"GOTCHA_PROBE_KEY":            "ptok",
		"GOTCHA_PROBE_SERVER_URL":     "https://gotcha.example.com",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode", "ingest"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "ingest" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "ingest")
	}
	if cfg.Addr != ":9090" || cfg.BaseURL != "https://errors.example.com" {
		t.Errorf("Addr/BaseURL not overridden: %q %q", cfg.Addr, cfg.BaseURL)
	}
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPPort != 465 ||
		cfg.SMTPUser != "mailer" || cfg.SMTPPassword != "s3cret" ||
		cfg.SMTPFrom != "gotcha@example.com" {
		t.Errorf("SMTP not overridden: %+v", cfg)
	}
	if cfg.RetentionDays != 30 || cfg.DefaultEventQuota != 50000 || cfg.MaxEventBytes != 2097152 {
		t.Errorf("numeric overrides failed: %+v", cfg)
	}
	if cfg.SpanRetentionDays != 7 {
		t.Errorf("SpanRetentionDays = %d, want 7", cfg.SpanRetentionDays)
	}
	if cfg.SecretKey != "prod-secret-at-least-32-bytes-long!" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
	if cfg.UptimeConcurrency != 10 {
		t.Errorf("UptimeConcurrency = %d, want 10", cfg.UptimeConcurrency)
	}
	if cfg.LocalRegion != "eu-fra" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "eu-fra")
	}
	if cfg.ProbeToken != "ptok" {
		t.Errorf("ProbeToken = %q, want %q", cfg.ProbeToken, "ptok")
	}
	if cfg.ServerURL != "https://gotcha.example.com" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "https://gotcha.example.com")
	}
}

// GOTCHA_TELEGRAM_API_BASE: пусто — дефолт пакета notify (api.telegram.org),
// непустое значение нормализуется. Хвостовая косая снимается, потому что
// отправитель дописывает «/bot{token}/sendMessage»: «…org/» дало бы «…org//bot…»
// и 404 на каждое уведомление.
func TestLoadConfigTelegramAPIBase(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.TelegramAPIBase != "" {
		t.Errorf("default TelegramAPIBase = %q, want empty", cfg.TelegramAPIBase)
	}
	for _, tc := range []struct{ in, want string }{
		{"https://tg.example.com", "https://tg.example.com"},
		{"https://tg.example.com/", "https://tg.example.com"},
		{"https://tg.example.com///", "https://tg.example.com"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"https://gw.example.com/telegram", "https://gw.example.com/telegram"},
	} {
		env := map[string]string{"GOTCHA_TELEGRAM_API_BASE": tc.in}
		cfg, err := loadConfig(getenvFrom(env), nil)
		if err != nil {
			t.Fatalf("GOTCHA_TELEGRAM_API_BASE=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.TelegramAPIBase != tc.want {
			t.Errorf("GOTCHA_TELEGRAM_API_BASE=%q: got %q, want %q", tc.in, cfg.TelegramAPIBase, tc.want)
		}
	}
}

// GOTCHA_BASE_URL нормализуется той же логикой, что GOTCHA_TELEGRAM_API_BASE
// выше (W3-D, запись 4): хвостовая косая снимается, потому что продукт сам
// дописывает путь к базе в heartbeat cron-команде, OAuth RedirectURI и
// ссылке-приглашении — «…app/» дало бы «…app//uptime/hb/…» в каждой из них.
func TestLoadConfigBaseURLNormalized(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("default BaseURL = %q, want %q", cfg.BaseURL, "http://localhost:8080")
	}
	for _, tc := range []struct{ in, want string }{
		{"https://gotcha.example.com", "https://gotcha.example.com"},
		{"https://gotcha.example.com/", "https://gotcha.example.com"},
		{"https://gotcha.example.com///", "https://gotcha.example.com"},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"https://gw.example.com/gotcha", "https://gw.example.com/gotcha"},
	} {
		// ALLOW_INSECURE_SECRET: сами значения не local (isLocalBaseURL) — без
		// него тест упёрся бы в ЧУЖУЮ проверку (слабый GOTCHA_SECRET_KEY на
		// не-local BaseURL, SEC-C1), не в ту, которую проверяет этот тест.
		env := map[string]string{"GOTCHA_BASE_URL": tc.in, "GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1"}
		cfg, err := loadConfig(getenvFrom(env), nil)
		if err != nil {
			t.Fatalf("GOTCHA_BASE_URL=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.BaseURL != tc.want {
			t.Errorf("GOTCHA_BASE_URL=%q: got %q, want %q", tc.in, cfg.BaseURL, tc.want)
		}
	}
}

// Невалидный GOTCHA_BASE_URL — отказ при запуске, а не битые ссылки в каждом
// письме/cron/редиректе: без схемы и хоста построенная ссылка ведёт в никуда.
func TestLoadConfigBaseURLRejectsInvalid(t *testing.T) {
	for _, v := range []string{
		"gotcha.example.com",
		"/app",
		"ftp://gotcha.example.com",
		"https://gotcha.example.com?token=1",
		"https://gotcha.example.com#frag",
		"https://",
	} {
		env := map[string]string{"GOTCHA_BASE_URL": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("GOTCHA_BASE_URL=%q: want error, got nil", v)
		}
	}
}

// Невалидный адрес Bot API — отказ при запуске, а не таймаут на каждой
// доставке: без схемы и хоста отправка падает с "unsupported protocol scheme",
// а запрос/фрагмент оказались бы посреди пути к /bot{token}/sendMessage.
func TestLoadConfigTelegramAPIBaseRejectsInvalid(t *testing.T) {
	for _, v := range []string{
		"tg.example.com",
		"/telegram",
		"ftp://tg.example.com",
		"https://tg.example.com?token=1",
		"https://tg.example.com#frag",
		"https://",
	} {
		env := map[string]string{"GOTCHA_TELEGRAM_API_BASE": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("GOTCHA_TELEGRAM_API_BASE=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigInvalidMode(t *testing.T) {
	if _, err := loadConfig(getenvFrom(nil), []string{"--mode", "banana"}); err == nil {
		t.Fatal("want error for invalid mode, got nil")
	}
}

func TestLoadConfigAcceptsUptimeAndProbeModes(t *testing.T) {
	// probe без GOTCHA_PROBE_SERVER_URL/GOTCHA_PROBE_KEY не запускается (см.
	// TestLoadConfigProbeModeRequiresServerURLAndToken), поэтому здесь они
	// заданы для обоих режимов — проверяется только разбор --mode.
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}
	for _, mode := range []string{"uptime", "probe"} {
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode", mode})
		if err != nil {
			t.Fatalf("mode %q: loadConfig: %v", mode, err)
		}
		if cfg.Mode != mode {
			t.Errorf("mode %q: Mode = %q, want %q", mode, cfg.Mode, mode)
		}
	}
}

func TestLoadConfigProbeModeRejectsServerURLWithoutScheme(t *testing.T) {
	// Без схемы/хоста каждый тик пробы падал бы с "unsupported protocol
	// scheme" раз в секунду вечно — отказываем на старте.
	for _, serverURL := range []string{"gotcha.example.com", "/probe", "ftp://gotcha.example.com"} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": serverURL,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: want error, got nil", serverURL)
		}
	}
}

// TestLoadConfigProbeServerURLNormalized — та же нормализация (E3 T6), что у
// GOTCHA_BASE_URL/GOTCHA_TELEGRAM_API_BASE: хвостовая косая срезается. До
// этой правки GOTCHA_PROBE_SERVER_URL хвостовую "/" не срезал вовсе.
//
// Ассерт — прямое наблюдаемое свойство cfg.ServerURL, а не синтетическая
// копия сборки запроса: internal/uptime/probeclient.go.post() уже режет
// хвостовой слэш САМ (strings.TrimSuffix(c.ServerURL, "/") + path) — точка
// использования подстрахована независимо от этого теста. Этот тест стережёт
// контракт КОНФИГУРАЦИИ (единая нормализация на старте, тот же baseurl.Normalize,
// что у остальных трёх базовых адресов), а не сборку URL запроса пробы —
// раньше здесь была конкатенация cfg.ServerURL+"/probe/lease" с проверкой
// "//probe" в результате, что проверяло собственную копию логики теста, а не
// боевой путь (round 1 ревью задачи 6: probeclient.go и без нормализации в
// конфиге не даёт двойной слэш благодаря своему TrimSuffix).
func TestLoadConfigProbeServerURLNormalized(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://gotcha.example.com", "https://gotcha.example.com"},
		{"https://gotcha.example.com/", "https://gotcha.example.com"},
		{"https://gotcha.example.com///", "https://gotcha.example.com"},
	} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": tc.in,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"})
		if err != nil {
			t.Fatalf("GOTCHA_PROBE_SERVER_URL=%q: loadConfig: %v", tc.in, err)
		}
		if cfg.ServerURL != tc.want {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: got %q, want %q", tc.in, cfg.ServerURL, tc.want)
		}
	}
}

// TestLoadConfigProbeServerURLRejectsQuery — та же политика query/fragment,
// что у GOTCHA_BASE_URL/GOTCHA_TELEGRAM_API_BASE, но только В --mode=probe:
// формат вне режима пробы не проверяется вовсе (см.
// TestLoadConfigProbeServerURLInvalidOutsideProbeModeOnlyWarns) — переменную
// никто не читает, и ронять по ней старт было бы тихим breaking change.
func TestLoadConfigProbeServerURLRejectsQuery(t *testing.T) {
	for _, v := range []string{
		"https://gotcha.example.com?token=1",
		"https://gotcha.example.com#frag",
	} {
		env := map[string]string{
			"GOTCHA_PROBE_SERVER_URL": v,
			"GOTCHA_PROBE_KEY":        "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: want error, got nil", v)
		}
	}
}

// TestLoadConfigProbeServerURLWarnsOutsideProbeMode — заданный, но
// бесполезный вне --mode=probe: ничего его не читает, поэтому старт не падает,
// но лог предупреждает — тот же паттерн, что GOTCHA_HSTS_* при выключенном
// GOTCHA_HSTS_ENABLED (TestLoadConfig_HSTSWarnings).
func TestLoadConfigProbeServerURLWarnsOutsideProbeMode(t *testing.T) {
	capture := func(t *testing.T, env map[string]string, args []string) ([]slog.Record, error) {
		t.Helper()
		var records []slog.Record
		prev := slog.Default()
		slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
		defer slog.SetDefault(prev)
		_, err := loadConfig(getenvFrom(env), args)
		return records, err
	}
	hasWarn := func(records []slog.Record, substr string) bool {
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
				return true
			}
		}
		return false
	}

	// Вне --mode=probe: предупреждение, старт продолжается.
	records, err := capture(t, map[string]string{"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com"}, nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("нет предупреждения о GOTCHA_PROBE_SERVER_URL вне --mode=probe")
	}

	// В --mode=probe — предупреждения нет, переменная используется по назначению.
	records, err = capture(t, map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}, []string{"--mode", "probe"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("предупреждение о GOTCHA_PROBE_SERVER_URL выдано в --mode=probe, где переменная используется")
	}

	// Не задан вовсе — тоже без предупреждения.
	records, err = capture(t, nil, nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if hasWarn(records, "GOTCHA_PROBE_SERVER_URL") {
		t.Error("предупреждение о GOTCHA_PROBE_SERVER_URL выдано при незаданной переменной")
	}
}

// TestLoadConfigProbeServerURLInvalidOutsideProbeModeOnlyWarns — round 1
// ревью задачи 6 (CRITICAL): на BASE (до унификации) GOTCHA_PROBE_SERVER_URL
// вне --mode=probe вообще не разбирался, и невалидное значение там СТАРТ НЕ
// РОНЯЛО — переменную в этом режиме никто не читает. Безусловный вызов
// baseurl.Normalize (без гейта режимом) превратил бы это в тихий breaking
// change: оператор с оставшимся от пробы или опечатанным значением перестал
// бы стартовать по значению, которое приложение и не собиралось читать. Формат
// проверяется ТОЛЬКО внутри --mode=probe (см. TestLoadConfigProbeServerURLRejectsQuery
// и TestLoadConfigProbeModeRejectsServerURLWithoutScheme); вне него —
// безусловно предупреждение и продолжение старта, независимо от валидности.
func TestLoadConfigProbeServerURLInvalidOutsideProbeModeOnlyWarns(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prev)

	for _, v := range []string{
		"not-a-url",
		"ftp://gotcha.example.com",
		"https://gotcha.example.com?token=1",
	} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROBE_SERVER_URL": v}), nil)
		if err != nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q вне --mode=probe: want no error, got %v", v, err)
			continue
		}
		if cfg.ServerURL != v {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q вне --mode=probe: ServerURL = %q, want непотронутое значение %q", v, cfg.ServerURL, v)
		}
	}
	found := false
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "GOTCHA_PROBE_SERVER_URL") {
			found = true
			break
		}
	}
	if !found {
		t.Error("нет предупреждения о GOTCHA_PROBE_SERVER_URL для невалидного значения вне --mode=probe")
	}
}

// TestLoadConfigDSNsRejectUnparseable — GOTCHA_PG_DSN/GOTCHA_CH_DSN до E3 T6
// не разбирались на старте вовсе: опечатка всплывала только на первом
// db.NewPostgres/db.NewClickHouse. Отказ должен называть переменную.
func TestLoadConfigDSNsRejectUnparseable(t *testing.T) {
	cases := []struct {
		key string
		env map[string]string
	}{
		{"GOTCHA_PG_DSN", map[string]string{"GOTCHA_PG_DSN": "::::"}},
		{"GOTCHA_CH_DSN", map[string]string{"GOTCHA_CH_DSN": "::::"}},
	}
	for _, tc := range cases {
		_, err := loadConfig(getenvFrom(tc.env), nil)
		if err == nil {
			t.Errorf("%s=::::: want error, got nil", tc.key)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=::::: error = %q, want it to name %s", tc.key, err, tc.key)
		}
	}
}

// TestLoadConfigDSNsAcceptKeywordValueForm — Postgres DSN легален и в
// keyword/value-форме (host=... user=... dbname=...), не только в URL-форме
// (postgres://...); обе формы принимает pgxpool.ParseConfig, поэтому и
// проверка на старте обязана принимать обе — иначе она отвергла бы легальный
// DSN, которым реально пользуются операторы.
func TestLoadConfigDSNsAcceptKeywordValueForm(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PG_DSN": "host=pg.example port=5432 user=gotcha password=s3cret dbname=gotcha sslmode=disable",
	}
	if _, err := loadConfig(getenvFrom(env), nil); err != nil {
		t.Errorf("keyword/value GOTCHA_PG_DSN: want no error, got %v", err)
	}
}

func TestLoadConfigNonPositiveUptimeConcurrency(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		env := map[string]string{"GOTCHA_UPTIME_CONCURRENCY": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Fatalf("GOTCHA_UPTIME_CONCURRENCY=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigInvalidInt(t *testing.T) {
	env := map[string]string{"GOTCHA_EVENT_RETENTION_DAYS": "ninety"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("want error for non-numeric GOTCHA_EVENT_RETENTION_DAYS, got nil")
	}
}

// №34: 0 = хранить вечно. Пол >= 1 снят у пяти переменных ретенции;
// документация (configuration.md) обещала это раньше, чем смог код.
func TestLoadConfigZeroRetentionMeansForever(t *testing.T) {
	env := map[string]string{
		"GOTCHA_EVENT_RETENTION_DAYS":    "0",
		"GOTCHA_SPAN_RETENTION_DAYS":     "0",
		"GOTCHA_METRIC_RETENTION_DAYS":   "0",
		"GOTCHA_PROFILE_RETENTION_DAYS":  "0",
		"GOTCHA_LOG_RETENTION_DAYS":      "0",
		"GOTCHA_INCIDENT_RETENTION_DAYS": "0",
		"GOTCHA_DEPLOY_RETENTION_DAYS":   "0",
	}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RetentionDays != 0 || cfg.SpanRetentionDays != 0 ||
		cfg.MetricRetentionDays != 0 || cfg.ProfileRetentionDays != 0 ||
		cfg.LogRetentionDays != 0 || cfg.IncidentRetentionDays != 0 ||
		cfg.DeployRetentionDays != 0 {
		t.Fatalf("want all retention fields = 0, got %d/%d/%d/%d/%d/%d/%d",
			cfg.RetentionDays, cfg.SpanRetentionDays, cfg.MetricRetentionDays,
			cfg.ProfileRetentionDays, cfg.LogRetentionDays, cfg.IncidentRetentionDays,
			cfg.DeployRetentionDays)
	}
}

// TestLoadConfigNegativeRetentionRejected — T2: GOTCHA_DEPLOY_RETENTION_DAYS
// добавлен в этот список. У шести соседей (RetentionDays, SpanRetentionDays,
// MetricRetentionDays, ProfileRetentionDays, LogRetentionDays,
// IncidentRetentionDays) проверка "< 0" была; у DeployRetentionDays — не
// было: отрицательное значение проходило validate() молча и попадало в
// time.Duration(cfg.DeployRetentionDays) * 24 * time.Hour (main.go) как
// отрицательный TTL.
func TestLoadConfigNegativeRetentionRejected(t *testing.T) {
	for _, key := range []string{
		"GOTCHA_EVENT_RETENTION_DAYS",
		"GOTCHA_SPAN_RETENTION_DAYS",
		"GOTCHA_METRIC_RETENTION_DAYS",
		"GOTCHA_PROFILE_RETENTION_DAYS",
		"GOTCHA_LOG_RETENTION_DAYS",
		"GOTCHA_INCIDENT_RETENTION_DAYS",
		"GOTCHA_DEPLOY_RETENTION_DAYS",
	} {
		env := map[string]string{key: "-1"}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Errorf("%s=-1: want error, got nil", key)
		}
	}
}

func TestLoadConfigNonPositiveDefaultEventQuota(t *testing.T) {
	// 0 = безлимит (разрешено); ошибка только на отрицательном значении.
	env := map[string]string{"GOTCHA_DEFAULT_EVENT_QUOTA": "-1"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("GOTCHA_DEFAULT_EVENT_QUOTA=-1: want error, got nil")
	}
}

func TestLoadConfigNonPositiveMaxEventBytes(t *testing.T) {
	env := map[string]string{"GOTCHA_MAX_EVENT_BYTES": "-1"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("GOTCHA_MAX_EVENT_BYTES=-1: want error, got nil")
	}
}

func TestLoadConfigProbeModeRequiresServerURLAndToken(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"both missing", nil},
		{"no token", map[string]string{"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com"}},
		{"no server url", map[string]string{"GOTCHA_PROBE_KEY": "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConfig(getenvFrom(tc.env), []string{"--mode=probe"}); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestLoadConfigProbeMode(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "probe-token",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=probe"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != "probe" {
		t.Errorf("Mode = %q, want probe", cfg.Mode)
	}
	if cfg.ServerURL != "https://gotcha.example.com" || cfg.ProbeToken != "probe-token" {
		t.Errorf("ServerURL = %q, ProbeToken set = %v", cfg.ServerURL, cfg.ProbeToken != "")
	}
}

// Остальные режимы GOTCHA_PROBE_SERVER_URL/GOTCHA_PROBE_KEY не требуют.
func TestLoadConfigNonProbeModeDoesNotRequireProbeCreds(t *testing.T) {
	for _, mode := range []string{"ingest", "web", "uptime", "all"} {
		if _, err := loadConfig(getenvFrom(nil), []string{"--mode=" + mode}); err != nil {
			t.Errorf("--mode=%s: %v", mode, err)
		}
	}
}

func TestLoadConfigOAuthProviders(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":         "true",
		"GOTCHA_OIDC_ISSUER":          "https://idp.example/realms/x",
		"GOTCHA_OIDC_CLIENT_ID":       "cid",
		"GOTCHA_OIDC_CLIENT_SECRET":   "sec",
		"GOTCHA_OIDC_DISPLAY_NAME":    "Corp SSO",
		"GOTCHA_YANDEX_ENABLED":       "true",
		"GOTCHA_YANDEX_CLIENT_ID":     "ycid",
		"GOTCHA_YANDEX_CLIENT_SECRET": "ysec",
	}
	cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.OIDCEnabled || cfg.OIDCIssuer != "https://idp.example/realms/x" || cfg.OIDCName != "Corp SSO" {
		t.Fatalf("OIDC fields = %+v", cfg)
	}
	if !cfg.YandexEnabled || cfg.YandexClientID != "ycid" {
		t.Fatalf("Yandex fields = %+v", cfg)
	}
	if cfg.VKEnabled {
		t.Fatalf("VK must be disabled")
	}
}

func TestLoadConfigOAuthMissingSecretFails(t *testing.T) {
	env := map[string]string{
		"GOTCHA_OIDC_ENABLED":   "true",
		"GOTCHA_OIDC_ISSUER":    "https://idp.example",
		"GOTCHA_OIDC_CLIENT_ID": "cid",
		// нет CLIENT_SECRET
	}
	if _, err := loadConfig(getenvFrom(env), []string{"--mode=all"}); err == nil {
		t.Fatal("enabled OIDC without secret must fail at startup")
	}
}

func TestLoadConfigProfileDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProfileRetentionDays != 7 {
		t.Errorf("ProfileRetentionDays = %d, want 7", cfg.ProfileRetentionDays)
	}
	if cfg.DefaultProfileQuota != 0 {
		t.Errorf("DefaultProfileQuota = %d, want 0 (oss unlimited)", cfg.DefaultProfileQuota)
	}
}

// №35: per-DSN лимит приёма настраивается; 0 выключает, дефолт 500 совпадает
// с прежним захардкоженным defaultIngestRatePerSec.
func TestLoadConfigIngestRateLimit(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.IngestRateLimit != 500 {
		t.Fatalf("default IngestRateLimit = %d, want 500", cfg.IngestRateLimit)
	}
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_RATE_PER_SEC": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig with 0: %v", err)
	}
	if cfg.IngestRateLimit != 0 {
		t.Fatalf("IngestRateLimit = %d, want 0", cfg.IngestRateLimit)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_INGEST_RATE_PER_SEC": "-1"}), nil); err == nil {
		t.Fatal("GOTCHA_INGEST_RATE_PER_SEC=-1: want error, got nil")
	}
}

func TestLoadConfigOutboxRetention(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.OutboxRetentionDays != 7 {
		t.Errorf("OutboxRetentionDays = %d, want 7", cfg.OutboxRetentionDays)
	}
	// Outbox — рабочая очередь, не архив: «хранить вечно» = неограниченный
	// рост таблицы, поэтому пол >= 1 сохранён и после №34 (0 = бессрочно
	// у переменных ретенции телеметрии).
	for _, v := range []string{"0", "-1"} {
		if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_OUTBOX_RETENTION_DAYS": v}), nil); err == nil {
			t.Errorf("GOTCHA_OUTBOX_RETENTION_DAYS=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfig_RejectsDefaultSecretInProd(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL": "https://gotcha.example.com",
		// GOTCHA_SECRET_KEY не задан → дефолт insecure-dev-secret
	}
	getenv := func(k string) string { return env[k] }
	_, err := loadConfig(getenv, []string{"--mode=all"})
	if err == nil {
		t.Fatal("expected error for default secret on non-local prod base url, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY") {
		t.Fatalf("error should mention GOTCHA_SECRET_KEY, got: %v", err)
	}
}

func TestLoadConfig_AllowsDefaultSecretOnLocalhost(t *testing.T) {
	getenv := func(k string) string { return "" } // всё дефолтное, BaseURL=localhost
	if _, err := loadConfig(getenv, []string{"--mode=all"}); err != nil {
		t.Fatalf("localhost dev must be allowed with default secret, got: %v", err)
	}
}

func TestLoadConfig_AllowsDefaultSecretWithEscapeHatch(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}
	getenv := func(k string) string { return env[k] }
	if _, err := loadConfig(getenv, []string{"--mode=all"}); err != nil {
		t.Fatalf("escape hatch must allow default secret, got: %v", err)
	}
}

// TestLoadConfig_SecretRequiredInDecryptingModes — мастер-ключ обязателен во ВСЕХ
// режимах, которые расшифровывают секреты at-rest, а не только там, где есть
// cookie.
//
// Раньше проверялись только web и all. Но тем же ключом зашифрованы секреты
// каналов доставки, а расшифровывают их теперь ingest и uptime тоже: секрет
// резолвится в момент отправки, а очередь наполняют оценщики из ingest и
// детектор аптайма. Реплика --mode=ingest с дефолтным ключом стартовала молча и
// отдавала в Telegram сырой шифротекст "enc:…" вместо bot-токена — 401 и вечные
// ретраи, не диагностируемые по симптому.
func TestLoadConfig_SecretRequiredInDecryptingModes(t *testing.T) {
	for _, mode := range []string{"web", "all", "ingest", "uptime"} {
		env := map[string]string{"GOTCHA_BASE_URL": "https://gotcha.example.com"}
		getenv := func(k string) string { return env[k] }
		if _, err := loadConfig(getenv, []string{"--mode=" + mode}); err == nil {
			t.Errorf("режим %s стартовал с дефолтным ключом на не-локальном URL", mode)
		}
	}

	// probe секретов не видит: ни PG, ни CH, ни каналов — ключ ему не нужен.
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "ptok",
	}
	getenv := func(k string) string { return env[k] }
	if _, err := loadConfig(getenv, []string{"--mode=probe"}); err != nil {
		t.Errorf("probe не расшифровывает секреты, ключ не нужен: %v", err)
	}
}

// TestLoadConfig_SecretErrorNotMaskedByUnrelatedTypo — ops P2-1: раньше
// проверка GOTCHA_SECRET_KEY шла хвостом функции, ПОСЛЕ бейл-аута по
// errs[0] на числовых/булевых полях (config.go). Если оператор одновременно
// опечатался в GOTCHA_EVENT_RETENTION_DAYS И оставил слабый/дефолтный секрет на
// проде, он видел только ошибку про retention, чинил её, перезапускал — и
// только тогда узнавал про секрет. Секьюрити-критичная проверка теперь идёт
// раньше errs[0], поэтому именно она должна быть видна первой.
func TestLoadConfig_SecretErrorNotMaskedByUnrelatedTypo(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":             "https://gotcha.example.com",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc", // опечатка в числовом поле
		// GOTCHA_SECRET_KEY не задан → дефолт insecure-dev-secret на не-local URL
	}
	getenv := func(k string) string { return env[k] }
	_, err := loadConfig(getenv, []string{"--mode=all"})
	if err == nil {
		t.Fatal("expected error (both a numeric typo and a weak secret are present), got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY") {
		t.Fatalf("the security-relevant secret error must surface even with an unrelated config typo present, got: %v", err)
	}
}

// TestLoadConfig_SecretKeyPrevRejectsInconsistentPairs — GOTCHA_SECRET_KEY_PREV
// задаёт предыдущий мастер-ключ на время ротации. Три сочетания с текущим
// ключом физически не могут означать «ротация идёт», и отказ старта тут —
// не про стойкость ключа (как у GOTCHA_SECRET_KEY_ALLOW_INSECURE), а про то, что
// молча проигнорированная переменная в .env убедила бы оператора в ротации,
// которой на самом деле нет.
func TestLoadConfig_SecretKeyPrevRejectsInconsistentPairs(t *testing.T) {
	const strongCurrent = "current-master-key-at-least-32-bytes!!"
	const strongOther = "other-master-key-also-32-bytes-long!!!"

	t.Run("PREV равен дев-ключу", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": devSecretKey,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("PREV == дев-ключ: want error, got nil")
		}
		// Фрагмент уникален для ветки «PREV == дев-ключ»: остальные две ветки
		// его не содержат, поэтому переклейка текстов ошибок между ветками
		// этот тест уронит (см. мутацию в отчёте задачи).
		if !strings.Contains(err.Error(), "public dev default") {
			t.Errorf("error = %q, want it to mention that PREV cannot be the public dev default", err)
		}
	})

	t.Run("PREV равен текущему ключу", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": strongCurrent,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("PREV == текущий ключ: want error, got nil")
		}
		// Фрагмент уникален для ветки «PREV == текущий ключ».
		if !strings.Contains(err.Error(), "differ from GOTCHA_SECRET_KEY") {
			t.Errorf("error = %q, want it to mention that PREV must differ from GOTCHA_SECRET_KEY", err)
		}
	})

	t.Run("текущий ключ дев, PREV задан", func(t *testing.T) {
		// Локальный BaseURL, чтобы не упереться в ЧУЖУЮ проверку (дефолтный
		// ключ на не-локальном URL) раньше, чем в свою.
		env := map[string]string{
			"GOTCHA_BASE_URL":        "http://localhost:8080",
			"GOTCHA_SECRET_KEY_PREV": strongOther,
		}
		_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err == nil {
			t.Fatal("текущий ключ дев + PREV задан: want error, got nil")
		}
		// Фрагмент уникален для ветки «текущий ключ ещё дев».
		if !strings.Contains(err.Error(), "still the dev default") {
			t.Errorf("error = %q, want it to mention that GOTCHA_SECRET_KEY is still the dev default", err)
		}
	})

	t.Run("корректная пара стартует", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": strongOther,
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("корректная пара ключей должна стартовать: %v", err)
		}
		if cfg.SecretKeyPrev != strongOther {
			t.Errorf("SecretKeyPrev = %q, want %q", cfg.SecretKeyPrev, strongOther)
		}
	})

	t.Run("пустой PREV — норма", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":   "https://gotcha.example.com",
			"GOTCHA_SECRET_KEY": strongCurrent,
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("пустой PREV — ротации нет, старт должен пройти: %v", err)
		}
		if cfg.SecretKeyPrev != "" {
			t.Errorf("SecretKeyPrev = %q, want пусто", cfg.SecretKeyPrev)
		}
	})

	t.Run("короткий PREV — норма, порога нет", func(t *testing.T) {
		env := map[string]string{
			"GOTCHA_BASE_URL":        "http://localhost:8080",
			"GOTCHA_SECRET_KEY":      strongCurrent,
			"GOTCHA_SECRET_KEY_PREV": "short-prev-key",
		}
		cfg, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
		if err != nil {
			t.Fatalf("короткий PREV не про стойкость: порога длины нет: %v", err)
		}
		if cfg.SecretKeyPrev != "short-prev-key" {
			t.Errorf("SecretKeyPrev = %q, want short-prev-key", cfg.SecretKeyPrev)
		}
	})
}

// TestLoadConfig_SecretKeyPrevAllowInsecureDoesNotBypass —
// GOTCHA_SECRET_KEY_ALLOW_INSECURE снимает требования к СТОЙКОСТИ ключа (дефолтный,
// короткий), а не к ЛОГИЧЕСКОЙ согласованности пары current/PREV: конфиг,
// который физически не может делать то, что от него ждут, «разрешать» нечего.
func TestLoadConfig_SecretKeyPrevAllowInsecureDoesNotBypass(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_PREV":           "some-other-strong-previous-key-32b!!",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
		// GOTCHA_SECRET_KEY не задан → дефолт insecure-dev-secret, что само по
		// себе разрешено эскейп-хэтчем, но с заданным PREV ротация невозможна
		// (дев-ключом ничего не шифровалось), и эскейп-хэтч это не чинит.
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err == nil {
		t.Fatal("GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 не должен снимать проверку PREV: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_PREV") {
		t.Errorf("error = %q, want it to mention GOTCHA_SECRET_KEY_PREV", err)
	}
}

// TestLoadConfig_SecretKeyPrevNotCheckedInProbeMode — probe не расшифровывает
// секретов вообще (см. secretKeyMattersFor), поэтому мёртвая для него
// переменная не должна ронять зонд — ровно то же правило, по которому в probe
// не проверяется и сам GOTCHA_SECRET_KEY.
func TestLoadConfig_SecretKeyPrevNotCheckedInProbeMode(t *testing.T) {
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_KEY":        "ptok",
		// PREV равен дев-ключу — в любом другом режиме это отказ старта.
		"GOTCHA_SECRET_KEY_PREV": devSecretKey,
	}
	if _, err := loadConfig(getenvFrom(env), []string{"--mode=probe"}); err != nil {
		t.Fatalf("probe не проверяет PREV, ключ ему не нужен: %v", err)
	}
}

func TestLoadConfigProfileEvalInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProfileEvalInterval != 300 {
		t.Errorf("ProfileEvalInterval = %d, want 300", cfg.ProfileEvalInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero profile eval interval must fail")
	}
}

func TestLoadConfigHostEvalInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HostEvalInterval != 60 {
		t.Errorf("HostEvalInterval = %d, want 60", cfg.HostEvalInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_HOST_EVAL_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero host eval interval must fail")
	}
}

func TestLoadConfigEscalationInterval(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.EscalationInterval != 60 {
		t.Errorf("EscalationInterval = %d, want 60", cfg.EscalationInterval)
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_ESCALATION_INTERVAL_SECONDS": "0"}), nil); err == nil {
		t.Error("zero escalation interval must fail")
	}
}

func TestLoadConfig_Registration(t *testing.T) {
	// Дефолт — invite.
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RegistrationMode != "invite" {
		t.Errorf("RegistrationMode default = %q, want %q", cfg.RegistrationMode, "invite")
	}
	// Явные допустимые значения.
	for _, mode := range []string{"open", "invite", "closed"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION_MODE": mode}), nil)
		if err != nil {
			t.Fatalf("loadConfig %q: %v", mode, err)
		}
		if cfg.RegistrationMode != mode {
			t.Errorf("RegistrationMode = %q, want %q", cfg.RegistrationMode, mode)
		}
	}
	// Мусорное значение — ошибка.
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION_MODE": "bogus"}), nil); err == nil {
		t.Error("bogus registration mode must fail")
	}
}

func TestLoadConfig_Locale(t *testing.T) {
	// Дефолт — ru: сохраняет сегодняшний язык регрессионных уведомлений
	// для действующих инсталляций.
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Locale != "ru" {
		t.Errorf("Locale default = %q, want %q", cfg.Locale, "ru")
	}
	// Явные допустимые значения.
	for _, loc := range []string{"ru", "en"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_LOCALE": loc}), nil)
		if err != nil {
			t.Fatalf("loadConfig %q: %v", loc, err)
		}
		if cfg.Locale != loc {
			t.Errorf("Locale = %q, want %q", cfg.Locale, loc)
		}
	}
	// Мусорное значение — ошибка.
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_LOCALE": "de"}), nil); err == nil {
		t.Error("bogus locale must fail")
	}
}

func TestLoadConfig_Edition(t *testing.T) {
	// Без env: OSS-редакция, все дефолты квот = 0 (безлимит), и это разрешено.
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Edition != "oss" {
		t.Errorf("Edition default = %q, want %q", cfg.Edition, "oss")
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota (oss) = %d, want 0", cfg.DefaultEventQuota)
	}
	if cfg.DefaultTransactionQuota != 0 || cfg.DefaultMetricQuota != 0 || cfg.DefaultProfileQuota != 0 || cfg.DefaultLogQuota != 0 {
		t.Errorf("oss quotas not all 0: tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	// SaaS-редакция: дефолты квот = 1_000_000.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "saas"}), nil)
	if err != nil {
		t.Fatalf("loadConfig saas: %v", err)
	}
	if cfg.Edition != "saas" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "saas")
	}
	if cfg.DefaultEventQuota != 1_000_000 {
		t.Errorf("DefaultEventQuota (saas) = %d, want 1000000", cfg.DefaultEventQuota)
	}
	if cfg.DefaultTransactionQuota != 1_000_000 || cfg.DefaultMetricQuota != 1_000_000 || cfg.DefaultProfileQuota != 1_000_000 || cfg.DefaultLogQuota != 1_000_000 {
		t.Errorf("saas quotas not all 1000000: tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	// 0 = безлимит — легитимная конфигурация в любой редакции, включая saas.
	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION":             "saas",
		"GOTCHA_DEFAULT_EVENT_QUOTA": "0",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig saas+0: unlimited must be allowed, got: %v", err)
	}
	if cfg.DefaultEventQuota != 0 {
		t.Errorf("DefaultEventQuota = %d, want 0", cfg.DefaultEventQuota)
	}

	// Явные env-переопределения всех пяти дефолтов.
	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_DEFAULT_EVENT_QUOTA":       "10",
		"GOTCHA_DEFAULT_TRANSACTION_QUOTA": "20",
		"GOTCHA_DEFAULT_METRIC_QUOTA":      "30",
		"GOTCHA_DEFAULT_PROFILE_QUOTA":     "40",
		"GOTCHA_DEFAULT_LOG_QUOTA":         "50",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig overrides: %v", err)
	}
	if cfg.DefaultEventQuota != 10 || cfg.DefaultTransactionQuota != 20 ||
		cfg.DefaultMetricQuota != 30 || cfg.DefaultProfileQuota != 40 || cfg.DefaultLogQuota != 50 {
		t.Errorf("quota overrides failed: event=%d tx=%d metric=%d profile=%d log=%d",
			cfg.DefaultEventQuota, cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
	}

	// Отрицательная квота — ошибка (0 разрешён, <0 нет).
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DEFAULT_METRIC_QUOTA": "-1"}), nil); err == nil {
		t.Error("negative GOTCHA_DEFAULT_METRIC_QUOTA must fail")
	}
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DEFAULT_LOG_QUOTA": "-1"}), nil); err == nil {
		t.Error("negative GOTCHA_DEFAULT_LOG_QUOTA must fail")
	}

	// Мусорная редакция — ошибка.
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "bogus"}), nil); err == nil {
		t.Error("bogus GOTCHA_EDITION must fail")
	}
}

func TestLoadConfig_Scrub(t *testing.T) {
	// Без env: PII-scrubbing включён по умолчанию, есть непустой denylist.
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.ScrubIP {
		t.Error("ScrubIP default = false, want true")
	}
	if !cfg.ScrubEmail {
		t.Error("ScrubEmail default = false, want true")
	}
	if len(cfg.ScrubKeys) == 0 {
		t.Error("ScrubKeys default is empty, want non-empty")
	}

	// Явное выключение флага.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_IP": "false"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ScrubIP {
		t.Error("ScrubIP = true с GOTCHA_SCRUB_IP=false, want false")
	}
	if !cfg.ScrubEmail {
		t.Error("ScrubEmail не должен зависеть от GOTCHA_SCRUB_IP")
	}

	// Пользовательский CSV-список ДОПОЛНЯЕТ дефолты, а не заменяет их: иначе
	// добавление одного своего поля молча снимало скрубинг с password/token/cvv.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_DENY_KEYS": "a,b"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasAll(cfg.ScrubKeys, defaultScrubKeys()) {
		t.Errorf("ScrubKeys = %v, дефолтный denylist потерян", cfg.ScrubKeys)
	}
	if !hasAll(cfg.ScrubKeys, []string{"a", "b"}) {
		t.Errorf("ScrubKeys = %v, пользовательские ключи не добавлены", cfg.ScrubKeys)
	}

	// Значение из одних разделителей не должно обнулять denylist: раньше ",,"
	// проходило проверку на непустоту, все элементы отсеивались, а ветка с
	// дефолтами пропускалась — скрубинг ключей выключался целиком и молча.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_DENY_KEYS": ",,"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasAll(cfg.ScrubKeys, defaultScrubKeys()) {
		t.Errorf("ScrubKeys = %v при GOTCHA_SCRUB_DENY_KEYS=\",,\" — denylist обнулён", cfg.ScrubKeys)
	}
}

// hasAll — все want присутствуют в got.
func hasAll(got, want []string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestTrustedRecipientsParsed — список доменов своего контура разбирается из
// запятых, чистится от пробелов и регистра, пустые записи отбрасываются.
func TestTrustedRecipientsParsed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_TRUSTED_RECIPIENTS": " Corp.Example , ,ops.example,",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"corp.example", "ops.example"}
	if len(cfg.TrustedRecipients) != len(want) {
		t.Fatalf("TrustedRecipients = %v, want %v", cfg.TrustedRecipients, want)
	}
	for i, w := range want {
		if cfg.TrustedRecipients[i] != w {
			t.Errorf("TrustedRecipients[%d] = %q, want %q", i, cfg.TrustedRecipients[i], w)
		}
	}
}

// TestTrustedRecipientsEmptyByDefault — без настройки список пуст: доверие
// хосту инстанса политика выводит сама, конфиг тут ничего не подставляет.
func TestTrustedRecipientsEmptyByDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.TrustedRecipients) != 0 {
		t.Fatalf("TrustedRecipients = %v, want empty", cfg.TrustedRecipients)
	}
}

// TestMigrateOnlyImpliesAutoMigrate — флаг существует ради применения
// миграций, поэтому он их и включает.
//
// Иначе `--migrate-only` вместе с GOTCHA_AUTO_MIGRATE_ENABLED=false — а это ровно та
// конфигурация, для которой флаг и нужен, — только проверил бы схему и вышел,
// ничего не применив.
func TestMigrateOnlyImpliesAutoMigrate(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_AUTO_MIGRATE_ENABLED": "false"}), []string{"--migrate-only"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.MigrateOnly {
		t.Error("MigrateOnly = false, want true")
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate = false при --migrate-only: запуск не применит миграции")
	}
}

// TestMigrateOnlyRejectedForProbe — проба не открывает базу вовсе, и молча
// выйти нулём было бы обманом: оператор решил бы, что схема применена.
func TestMigrateOnlyRejectedForProbe(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_KEY": "t",
	}), []string{"--migrate-only", "--mode=probe"})
	if err == nil {
		t.Fatal("loadConfig принял --migrate-only с --mode=probe")
	}
	if !strings.Contains(err.Error(), "migrate-only") {
		t.Errorf("ошибка = %v, want упоминание --migrate-only", err)
	}
}

// TestMigrateOnlyDefaultsOff — обычный запуск флага не несёт.
func TestMigrateOnlyDefaultsOff(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateOnly {
		t.Error("MigrateOnly = true без флага")
	}
}

// TestMigrateForceFlags — разбор --migrate-force/--migrate-force-ch: три
// несовместимости отклоняются на разборе конфигурации, одиночный флаг
// разбирается, дефолт — «не запрошено» (−1).
func TestMigrateForceFlags(t *testing.T) {
	probeEnv := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_KEY": "t",
	}
	rejected := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"с probe: база не открывается", probeEnv, []string{"--migrate-force=57", "--mode=probe"}},
		{"с migrate-only: разные намерения", nil, []string{"--migrate-force=57", "--migrate-only"}},
		{"оба разом: застрять могла одна база", nil, []string{"--migrate-force=57", "--migrate-force-ch=3"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(tc.env), tc.args)
			if err == nil {
				t.Fatalf("loadConfig принял %v", tc.args)
			}
			if !strings.Contains(err.Error(), "migrate-force") {
				t.Errorf("ошибка = %v, want упоминание --migrate-force", err)
			}
		})
	}

	cfg, err := loadConfig(getenvFrom(nil), []string{"--migrate-force-ch=12"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateForceCH != 12 || cfg.MigrateForcePG != -1 {
		t.Errorf("MigrateForceCH=%d MigrateForcePG=%d, want 12 и -1", cfg.MigrateForceCH, cfg.MigrateForcePG)
	}

	cfg, err = loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MigrateForcePG != -1 || cfg.MigrateForceCH != -1 {
		t.Errorf("дефолт: MigrateForcePG=%d MigrateForceCH=%d, want -1 и -1", cfg.MigrateForcePG, cfg.MigrateForceCH)
	}
}

// TestLoadConfigOutOfRangeNumericEnvRejected — интервалы, окна, лимиты и
// квоты, у которых свой пол. Проверка не только «отказ есть», но и «отказ про
// ЭТУ переменную»: сообщения писались копипастой соседнего блока, и оператор,
// получив на старте имя чужой переменной, чинит не то, что сломано.
func TestLoadConfigOutOfRangeNumericEnvRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_ALERT_BUDGET_WINDOW_SECONDS", "0"},
		{"GOTCHA_ALERT_BUDGET_LIMIT", "-1"},
		{"GOTCHA_CARDINALITY_LIMIT", "-1"},
		{"GOTCHA_CARDINALITY_WINDOW_SECONDS", "0"},
		{"GOTCHA_METRIC_EVAL_INTERVAL_SECONDS", "0"},
		{"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS", "-1"},
		{"GOTCHA_NOTIFY_CONCURRENCY", "0"},
		{"GOTCHA_SLO_EVAL_INTERVAL_SECONDS", "0"},
		{"GOTCHA_DEPENDENCY_SETTLE_SECONDS", "-1"},
		{"GOTCHA_DEFAULT_TRANSACTION_QUOTA", "-1"},
		{"GOTCHA_DEFAULT_PROFILE_QUOTA", "-1"},
		{"GOTCHA_SMTP_PORT", "-1"},
		{"GOTCHA_SMTP_PORT", "0"},
		{"GOTCHA_SMTP_PORT", "99999"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
		})
	}
}

// TestLoadConfigRejectsInvalidBoolean — мусор в булевой переменной не должен
// молча превращаться в false: «GOTCHA_SCRUB_IP=maybe» так выключил бы
// скрубинг IP, а оператор считал бы его включённым.
func TestLoadConfigRejectsInvalidBoolean(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_IP": "maybe"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_SCRUB_IP=maybe: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SCRUB_IP") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_SCRUB_IP and say 'invalid boolean'", err)
	}
}

// TestLoadConfigRejectsNonNumericInt64 — байтовые потолки читаются как целое;
// «8MiB» разбирается в 0, а 0 для GOTCHA_MAX_WRITER_BUFFER_BYTES означает «выведи
// потолок сам» (см. effectiveMaxBufferBytes), то есть опечатка тихо меняла бы
// смысл настройки вместо отказа на старте.
func TestLoadConfigRejectsNonNumericInt64(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_MAX_WRITER_BUFFER_BYTES": "8MiB"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_MAX_WRITER_BUFFER_BYTES=8MiB: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_WRITER_BUFFER_BYTES") {
		t.Errorf("error = %q, want it to name GOTCHA_MAX_WRITER_BUFFER_BYTES", err)
	}
}

// TestParseInt64EnvReturnsDefOnParseError — проверяет parseInt64Env напрямую,
// в обход loadConfig: errors.Join(errs...) в loadConfig возвращается раньше
// всех cfg.X-валидаций, поэтому разница между def и частичным результатом
// strconv.ParseInt («8MiB» → 0 при синтаксической ошибке, переполнение →
// значение, зажатое до края int64) снаружи loadConfig ненаблюдаема ни одним
// тестом на самом Config — её обязана ловить проверка функции разбора.
func TestParseInt64EnvReturnsDefOnParseError(t *testing.T) {
	cases := []struct{ name, value string }{
		{"syntax", "8MiB"},
		{"overflow", "999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GOTCHA_TEST_INT64": tc.value}
			got, err := parseInt64Env(getenvFrom(env), "GOTCHA_TEST_INT64", 5)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.value)
			}
			if !strings.Contains(err.Error(), "GOTCHA_TEST_INT64") {
				t.Errorf("error = %q, want it to name GOTCHA_TEST_INT64", err)
			}
			if got != 5 {
				t.Errorf("got = %d, want def=5 (not a partial strconv.ParseInt result)", got)
			}
		})
	}
	// Незаданная переменная — не ошибка, просто def.
	if got, err := parseInt64Env(getenvFrom(nil), "GOTCHA_TEST_INT64", 5); err != nil || got != 5 {
		t.Errorf("unset: got (%d, %v), want (5, nil)", got, err)
	}
	// Валидное значение разбирается как есть, без ошибки.
	if got, err := parseInt64Env(getenvFrom(map[string]string{"GOTCHA_TEST_INT64": "42"}), "GOTCHA_TEST_INT64", 5); err != nil || got != 42 {
		t.Errorf("valid: got (%d, %v), want (42, nil)", got, err)
	}
}

// TestParseIntEnvReturnsDefOnParseError — тот же случай для parseIntEnv
// (полей типа int), см. TestParseInt64EnvReturnsDefOnParseError.
func TestParseIntEnvReturnsDefOnParseError(t *testing.T) {
	cases := []struct{ name, value string }{
		{"syntax", "8MiB"},
		{"overflow", "999999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GOTCHA_TEST_INT": tc.value}
			got, err := parseIntEnv(getenvFrom(env), "GOTCHA_TEST_INT", 7)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.value)
			}
			if !strings.Contains(err.Error(), "GOTCHA_TEST_INT") {
				t.Errorf("error = %q, want it to name GOTCHA_TEST_INT", err)
			}
			if got != 7 {
				t.Errorf("got = %d, want def=7 (not a partial strconv.ParseInt result)", got)
			}
		})
	}
	if got, err := parseIntEnv(getenvFrom(nil), "GOTCHA_TEST_INT", 7); err != nil || got != 7 {
		t.Errorf("unset: got (%d, %v), want (7, nil)", got, err)
	}
	if got, err := parseIntEnv(getenvFrom(map[string]string{"GOTCHA_TEST_INT": "13"}), "GOTCHA_TEST_INT", 7); err != nil || got != 13 {
		t.Errorf("valid: got (%d, %v), want (13, nil)", got, err)
	}
}

// TestLoadConfigMaxBufferAndQueueBytesZeroOrNegativeRejected —
// GOTCHA_MAX_WRITER_BUFFER_BYTES и GOTCHA_MAX_INGEST_QUEUE_BYTES вошли в семью
// «запрещённого нуля» вместе с OUTBOX_RETENTION_DAYS/MAX_EVENT_BYTES/
// *_EVAL_INTERVAL_SECONDS/*_WINDOW_SECONDS/*_CONCURRENCY: явный 0 или
// отрицательное значение — отказ старта, а не тихий откат к дефолту пакета
// (который для этих двух переменных тоже кодируется нулём — см.
// TestLoadConfigMaxBufferAndQueueBytesUnsetUsesPackageDefault).
func TestLoadConfigMaxBufferAndQueueBytesZeroOrNegativeRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_MAX_WRITER_BUFFER_BYTES", "0"},
		{"GOTCHA_MAX_WRITER_BUFFER_BYTES", "-1"},
		{"GOTCHA_MAX_INGEST_QUEUE_BYTES", "0"},
		{"GOTCHA_MAX_INGEST_QUEUE_BYTES", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
		})
	}
}

// TestLoadConfigMaxBufferAndQueueBytesUnsetUsesPackageDefault — переменная,
// оставленная незаданной, обязана вести себя как раньше: дефолт писателя
// (для буфера — через effectiveMaxBufferBytes, а не приватное поле писателя;
// для очереди — прямое значение Config.MaxQueueBytes, публичный контракт).
func TestLoadConfigMaxBufferAndQueueBytesUnsetUsesPackageDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := effectiveMaxBufferBytes(cfg.MaxBufferBytes, 0); got != 0 {
		t.Errorf("effectiveMaxBufferBytes(cfg.MaxBufferBytes, 0) = %d, want 0 (writer package default, no heap ceiling detected)", got)
	}
	if cfg.MaxQueueBytes != 0 {
		t.Errorf("MaxQueueBytes = %d, want 0 (queue package default) когда GOTCHA_MAX_INGEST_QUEUE_BYTES не задана", cfg.MaxQueueBytes)
	}
}

// TestLoadConfigGarbageInMultipleNumericVarsReportsAllNames — loadConfig
// обязан вернуть ВСЕ накопленные ошибки, а не первую (errs[0]): оператор с
// несколькими опечатками правит .env за один проход, а не за один деплой на
// каждую. Мутация: вернуть errs[0] вместо errors.Join(errs...) красит именно
// этот тест — в тексте ошибки исчезнет одно из двух имён.
func TestLoadConfigGarbageInMultipleNumericVarsReportsAllNames(t *testing.T) {
	env := map[string]string{
		"GOTCHA_MAX_WRITER_BUFFER_BYTES": "8MiB",
		"GOTCHA_MAX_INGEST_QUEUE_BYTES":  "not-a-number",
	}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("мусор в двух числовых переменных: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_WRITER_BUFFER_BYTES") {
		t.Errorf("error = %q, does not name GOTCHA_MAX_WRITER_BUFFER_BYTES", err)
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_INGEST_QUEUE_BYTES") {
		t.Errorf("error = %q, does not name GOTCHA_MAX_INGEST_QUEUE_BYTES", err)
	}
}

// TestLoadConfigSecretKeyErrorNotDrownedByNumericErrors — ops P2-1: слабый/
// дефолтный GOTCHA_SECRET_KEY на не-локальном BaseURL обязан быть виден
// оператору САМ ПО СЕБЕ, даже когда рядом одновременно опечатка в числовой
// переменной. Переход к «вернуть ВСЕ ошибки» не должен утопить security-
// critical предупреждение о ключе среди диагностики опечаток — секретный
// ключ проверяется своим отдельным return ДО сбора errs.
func TestLoadConfigSecretKeyErrorNotDrownedByNumericErrors(t *testing.T) {
	env := map[string]string{
		"GOTCHA_BASE_URL":             "https://gotcha.example",
		"GOTCHA_EVENT_RETENTION_DAYS": "abc",
	}
	_, err := loadConfig(getenvFrom(env), []string{"--mode=web"})
	if err == nil {
		t.Fatal("дефолтный GOTCHA_SECRET_KEY на не-локальном BaseURL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY must be set to a strong random value") {
		t.Errorf("error = %q, want the secret-key warning to be visible and not superseded by the retention-days typo", err)
	}
	if strings.Contains(err.Error(), "GOTCHA_EVENT_RETENTION_DAYS") {
		t.Errorf("error = %q, want ONLY the secret-key warning, not it plus the numeric typo (SEC-C1: secret must win, not share the line)", err)
	}
}

// TestLoadConfigTrustedProxiesParsed — CIDR и голые адреса. Голый IP обязан
// становиться /32 (/128 для IPv6), иначе net.ParseCIDR отвергнет запись, и
// самая частая форма записи («192.168.1.5») стала бы ошибкой конфигурации.
func TestLoadConfigTrustedProxiesParsed(t *testing.T) {
	env := map[string]string{
		"GOTCHA_TRUSTED_PROXIES": " 10.0.0.0/8 , 192.168.1.5 ,, 2001:db8::1 ",
	}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.TrustedProxies) != 3 {
		t.Fatalf("TrustedProxies = %v, want 3 entries (пустая запись отбрасывается)", cfg.TrustedProxies)
	}
	got := make([]string, 0, len(cfg.TrustedProxies))
	for _, n := range cfg.TrustedProxies {
		got = append(got, n.String())
	}
	want := []string{"10.0.0.0/8", "192.168.1.5/32", "2001:db8::1/128"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestLoadConfigTrustedProxiesRejectsInvalidEntry — невалидная запись обязана
// ронять старт, а не пропускаться молча: тихо выпавший из списка прокси
// означает, что X-Forwarded-For больше не доверяется, лимитер ключуется по
// адресу прокси и защита деградирует, ничего об этом не сообщая.
func TestLoadConfigTrustedProxiesRejectsInvalidEntry(t *testing.T) {
	env := map[string]string{"GOTCHA_TRUSTED_PROXIES": "10.0.0.0/8,not-an-ip"}
	_, err := loadConfig(getenvFrom(env), nil)
	if err == nil {
		t.Fatal("GOTCHA_TRUSTED_PROXIES с мусорной записью: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_TRUSTED_PROXIES") || !strings.Contains(err.Error(), "not-an-ip") {
		t.Errorf("error = %q, want it to name the variable and the bad entry", err)
	}
}

// TestLoadConfigScrubAllowKeysParsed — исключения из denylist сравниваются с
// именами полей в нижнем регистре, поэтому список нормализуется при разборе;
// без нормализации «Order_ID» не совпал бы ни с чем и исключение молча не
// работало бы.
func TestLoadConfigScrubAllowKeysParsed(t *testing.T) {
	env := map[string]string{"GOTCHA_SCRUB_KEEP_KEYS": " Order_ID , ,USER_ID"}
	cfg, err := loadConfig(getenvFrom(env), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.ScrubAllowKeys) != 2 {
		t.Fatalf("ScrubAllowKeys = %v, want 2 entries (пустая запись отбрасывается)", cfg.ScrubAllowKeys)
	}
	if cfg.ScrubAllowKeys[0] != "order_id" || cfg.ScrubAllowKeys[1] != "user_id" {
		t.Errorf("ScrubAllowKeys = %v, want [order_id user_id]", cfg.ScrubAllowKeys)
	}
}

// TestLoadConfigShortSecretKeyOnRemoteBaseURL — свой, но короткий ключ на
// не-локальном стенде: 16 байт подписи угадываемы, а тем же ключом шифруются
// секреты каналов доставки. Локальный стенд и явный аварийный тумблер —
// исключения.
func TestLoadConfigShortSecretKeyOnRemoteBaseURL(t *testing.T) {
	short := "0123456789abcdef" // 16 байт, ровно вдвое меньше требуемого
	base := map[string]string{
		"GOTCHA_BASE_URL":   "https://gotcha.example",
		"GOTCHA_SECRET_KEY": short,
	}
	_, err := loadConfig(getenvFrom(base), []string{"--mode=web"})
	if err == nil {
		t.Fatal("короткий GOTCHA_SECRET_KEY на не-локальном BaseURL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY is too short") {
		t.Errorf("error = %q, want it to say the key is too short", err)
	}

	// Явный аварийный тумблер снимает проверку.
	withEscape := map[string]string{
		"GOTCHA_BASE_URL":                  "https://gotcha.example",
		"GOTCHA_SECRET_KEY":                short,
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}
	if _, err := loadConfig(getenvFrom(withEscape), []string{"--mode=web"}); err != nil {
		t.Fatalf("GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 должен разрешать короткий ключ: %v", err)
	}

	// Локальный стенд — тоже исключение, тумблер там не нужен.
	local := map[string]string{
		"GOTCHA_BASE_URL":   "http://localhost:8080",
		"GOTCHA_SECRET_KEY": short,
	}
	if _, err := loadConfig(getenvFrom(local), []string{"--mode=web"}); err != nil {
		t.Fatalf("короткий ключ на localhost должен проходить: %v", err)
	}

	// probe секретов не расшифровывает — короткий ключ ему не мешает.
	probe := map[string]string{
		"GOTCHA_BASE_URL":         "https://gotcha.example",
		"GOTCHA_SECRET_KEY":       short,
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example",
		"GOTCHA_PROBE_KEY":        "tok",
	}
	if _, err := loadConfig(getenvFrom(probe), []string{"--mode=probe"}); err != nil {
		t.Fatalf("--mode=probe с коротким ключом должен проходить: %v", err)
	}
}

// TestLoadConfigRejectsUnparseableURLs — адрес, который не разбирается вовсе
// (незакрытая скобка IPv6-хоста). Ветка отдельная от проверки схемы/хоста:
// url.Parse возвращает ошибку раньше, чем есть что проверять.
func TestLoadConfigRejectsUnparseableURLs(t *testing.T) {
	const broken = "http://[::1"

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_BASE_URL": broken}), nil); err == nil {
		t.Error("GOTCHA_BASE_URL=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_BASE_URL") {
		t.Errorf("BaseURL error = %q, want it to name GOTCHA_BASE_URL", err)
	}

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_TELEGRAM_API_BASE": broken}), nil); err == nil {
		t.Error("GOTCHA_TELEGRAM_API_BASE=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_TELEGRAM_API_BASE") {
		t.Errorf("Telegram error = %q, want it to name GOTCHA_TELEGRAM_API_BASE", err)
	}

	probe := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": broken,
		"GOTCHA_PROBE_KEY":        "tok",
	}
	if _, err := loadConfig(getenvFrom(probe), []string{"--mode=probe"}); err == nil {
		t.Error("GOTCHA_PROBE_SERVER_URL=" + broken + ": want error, got nil")
	} else if !strings.Contains(err.Error(), "GOTCHA_PROBE_SERVER_URL") {
		t.Errorf("probe error = %q, want it to name GOTCHA_PROBE_SERVER_URL", err)
	}
}

// TestLoadConfigSocialProvidersRequireSecrets — включённый провайдер без
// client_id/secret. Стартовать с ним значит отдать пользователю кнопку входа,
// которая падает уже после редиректа к провайдеру.
func TestLoadConfigSocialProvidersRequireSecrets(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"yandex без id и секрета", map[string]string{"GOTCHA_YANDEX_ENABLED": "1"}},
		{"yandex без секрета", map[string]string{
			"GOTCHA_YANDEX_ENABLED":   "1",
			"GOTCHA_YANDEX_CLIENT_ID": "ycid",
		}},
		{"vk без id и секрета", map[string]string{"GOTCHA_VK_ENABLED": "1"}},
		{"vk без секрета", map[string]string{
			"GOTCHA_VK_ENABLED":   "1",
			"GOTCHA_VK_CLIENT_ID": "vcid",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConfig(getenvFrom(tc.env), []string{"--mode=web"}); err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
		})
	}

	// Полный комплект — стартуем.
	full := map[string]string{
		"GOTCHA_VK_ENABLED":       "1",
		"GOTCHA_VK_CLIENT_ID":     "vcid",
		"GOTCHA_VK_CLIENT_SECRET": "vsec",
	}
	cfg, err := loadConfig(getenvFrom(full), []string{"--mode=web"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.VKEnabled || cfg.VKClientSecret != "vsec" {
		t.Errorf("VK fields = %v/%q", cfg.VKEnabled, cfg.VKClientSecret)
	}
}

// TestLoadConfigRunEvaluatorsTriState — «не задано» и «задано false» означают
// разное (см. runEvaluatorsExplicit), поэтому поле тристабильное, и loadConfig
// обязан различать все три состояния.
func TestLoadConfigRunEvaluatorsTriState(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RunEvaluators != nil {
		t.Errorf("RunEvaluators = %v без переменной, want nil", *cfg.RunEvaluators)
	}

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"YES", true}, {"on", true}, {" on ", true},
		{"0", false}, {"false", false}, {"FALSE", false}, {"no", false}, {"NO", false}, {"off", false}, {" off ", false},
	} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EVALUATORS_ENABLED": tc.value}), nil)
		if err != nil {
			t.Fatalf("GOTCHA_EVALUATORS_ENABLED=%q: loadConfig: %v", tc.value, err)
		}
		if cfg.RunEvaluators == nil {
			t.Errorf("GOTCHA_EVALUATORS_ENABLED=%q: RunEvaluators = nil, want заданное значение", tc.value)
			continue
		}
		if *cfg.RunEvaluators != tc.want {
			t.Errorf("GOTCHA_EVALUATORS_ENABLED=%q: RunEvaluators = %v, want %v", tc.value, *cfg.RunEvaluators, tc.want)
		}
	}
}

// TestLoadConfigRunEvaluatorsRejectsInvalid — «ture» и подобный мусор обязаны
// ронять старт, а не тихо трактоваться как false: раньше RunEvaluators шёл в
// обход общего parseBool через отдельный разбор (optionalBoolEnv) и такие
// значения проглатывал молча.
func TestLoadConfigRunEvaluatorsRejectsInvalid(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EVALUATORS_ENABLED": "ture"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_EVALUATORS_ENABLED=ture: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_EVALUATORS_ENABLED") || !strings.Contains(err.Error(), "invalid boolean") {
		t.Errorf("error = %q, want it to name GOTCHA_EVALUATORS_ENABLED and say 'invalid boolean'", err)
	}
}

// sortedRenamedOldNames — старые имена envcontract.Renamed в детерминированном
// (алфавитном) порядке. Поведенческие тесты ниже намеренно не пишут ни одного
// старого имени буквально: TestNoRenamedEnvVarNames (internal/guards) иначе
// пришлось бы выводить из-под проверки весь этот файл, а не точечную
// фикстуру-справочник (см. cmd/gotcha/renamed_env_contract_test.go) — файл с
// сотнями других GOTCHA_-токенов растёт с каждой фичей конфига, и случайно
// оставленное там старое имя сторож обязан ловить. Вместо литералов тесты
// берут пару(ы) прямо из карты-истины: тест остаётся привязан к ней и не
// слабеет (проверяется механика отказа, а не конкретное имя), а вывод
// t.Errorf всё равно называет ИМЕННО ту пару, что проверялась, — она просто
// подставляется в текст ошибки как переменная, а не пишется в код теста.
func sortedRenamedOldNames() []string {
	names := make([]string, 0, len(envcontract.Renamed))
	for old := range envcontract.Renamed {
		names = append(names, old)
	}
	sort.Strings(names)
	return names
}

// renamedEnvVarNewNameChecks — новое имя → (тестовое значение, читатель
// соответствующего поля Config). TestLoadConfigRenamedEnvVarNewNameStillApplies
// ниже проходит ПО ВСЕЙ этой таблице подтестами, а не по одной записи —
// таблица, по которой реально не итерируются, ничем не отличается от
// комментария: ложная уверенность в покрытии всех переименований на деле
// покрывала бы одно. Полноту самой таблицы (что в ней ровно те новые имена,
// что есть в envcontract.Renamed, — ни лишних, ни пропущенных) проверяет
// TestRenamedEnvVarNewNameChecksComplete в renamed_env_contract_test.go.
var renamedEnvVarNewNameChecks = map[string]struct {
	value string
	get   func(Config) string
}{
	"GOTCHA_METRIC_EVAL_INTERVAL_SECONDS":  {"301", func(c Config) string { return strconv.Itoa(c.MetricEvalInterval) }},
	"GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS": {"302", func(c Config) string { return strconv.Itoa(c.ProfileEvalInterval) }},
	"GOTCHA_HOST_EVAL_INTERVAL_SECONDS":    {"303", func(c Config) string { return strconv.Itoa(c.HostEvalInterval) }},
	"GOTCHA_SLO_EVAL_INTERVAL_SECONDS":     {"304", func(c Config) string { return strconv.Itoa(c.SLOEvalInterval) }},
	"GOTCHA_ESCALATION_INTERVAL_SECONDS":   {"305", func(c Config) string { return strconv.Itoa(c.EscalationInterval) }},
	"GOTCHA_EVENT_RETENTION_DAYS":          {"306", func(c Config) string { return strconv.Itoa(c.RetentionDays) }},
	"GOTCHA_PROBE_SERVER_URL":              {"https://renamed-regression.example", func(c Config) string { return c.ServerURL }},
	"GOTCHA_INGEST_RATE_PER_SEC":           {"307", func(c Config) string { return strconv.Itoa(c.IngestRateLimit) }},
	"GOTCHA_DIST_DIR":                      {"/tmp/renamed-regression-dist", func(c Config) string { return c.AgentDistDir }},
	"GOTCHA_DIST_RATE_PER_MIN":             {"308", func(c Config) string { return strconv.Itoa(c.AgentDistRatePerMin) }},
	// E3, заморозка контракта
	"GOTCHA_LISTEN_ADDR":            {":9309", func(c Config) string { return c.Addr }},
	"GOTCHA_LOGGING_LEVEL":          {"debug", func(c Config) string { return c.LogLevel }},
	"GOTCHA_LOGGING_FORMAT":         {"json", func(c Config) string { return c.LogFormat }},
	"GOTCHA_UPTIME_LOCAL_REGION":    {"renamed-regression-region", func(c Config) string { return c.LocalRegion }},
	"GOTCHA_REGISTRATION_MODE":      {"open", func(c Config) string { return c.RegistrationMode }},
	"GOTCHA_EXPORT_RETENTION_HOURS": {"309", func(c Config) string { return strconv.Itoa(c.ExportTTLHours) }},
	"GOTCHA_SCRUB_DENY_KEYS": {"renamed_regression_deny_key", func(c Config) string {
		for _, k := range c.ScrubKeys {
			if k == "renamed_regression_deny_key" {
				return k
			}
		}
		return ""
	}},
	"GOTCHA_SCRUB_KEEP_KEYS": {"renamed_regression_keep_key", func(c Config) string {
		for _, k := range c.ScrubAllowKeys {
			if k == "renamed_regression_keep_key" {
				return k
			}
		}
		return ""
	}},
	"GOTCHA_EVALUATORS_ENABLED": {"true", func(c Config) string {
		if c.RunEvaluators == nil {
			return "nil"
		}
		return strconv.FormatBool(*c.RunEvaluators)
	}},
	"GOTCHA_AUTO_MIGRATE_ENABLED":             {"false", func(c Config) string { return strconv.FormatBool(c.AutoMigrate) }},
	"GOTCHA_SECRET_KEY_ALLOW_INSECURE":        {"true", func(c Config) string { return strconv.FormatBool(c.AllowInsecureSecret) }},
	"GOTCHA_MAX_WRITER_BUFFER_BYTES":          {"26214400", func(c Config) string { return strconv.FormatInt(c.MaxBufferBytes, 10) }},
	"GOTCHA_MAX_INGEST_QUEUE_BYTES":           {"10485760", func(c Config) string { return strconv.FormatInt(c.MaxQueueBytes, 10) }},
	"GOTCHA_PROBE_KEY":                        {"renamed-regression-probe-key", func(c Config) string { return c.ProbeToken }},
	"GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED": {"true", func(c Config) string { return strconv.FormatBool(c.ExternalChannelDetails) }},
	"GOTCHA_OIDC_DISPLAY_NAME":                {"Renamed Regression SSO", func(c Config) string { return c.OIDCName }},
	"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS":    {"309", func(c Config) string { return strconv.Itoa(c.PurgeReconcileHours) }},
}

// TestLoadConfigRenamedEnvVarFailsStart — старое имя переменной окружения с
// непустым значением роняет старт (envcontract.Renamed), а не молча
// подменяется дефолтом. Сообщение обязано называть И старое, И новое имя —
// оператор должен сразу понять, что чинить.
//
// Подтест на КАЖДУЮ пару реестра (t.Run по старому имени), а не одна
// проверка на sortedRenamedOldNames()[0]: взятие только алфавитно первой
// пары — вырожденное прочтение требования «итерация по envcontract.Renamed»
// и ровно тот анти-паттерн, на котором проект уже обжигался (таблица без
// реального обхода защищает одну строку из многих, а не все). При такой
// проверке неоднородный баг в envcontract.CheckRenamed — скажем, срабатывающий
// не на всех именах, — прошёл бы незамеченным: единственная запись под
// защитой не покрывает остальные двадцать шесть.
func TestLoadConfigRenamedEnvVarFailsStart(t *testing.T) {
	for _, old := range sortedRenamedOldNames() {
		newName := envcontract.Renamed[old]
		t.Run(old, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{old: "some-value"}), nil)
			if err == nil {
				t.Fatalf("loadConfig: want ошибку на устаревшем %s, получили nil", old)
			}
			if !strings.Contains(err.Error(), old) {
				t.Errorf("err = %q, want упоминание старого имени %s", err, old)
			}
			if !strings.Contains(err.Error(), newName) {
				t.Errorf("err = %q, want упоминание нового имени %s", err, newName)
			}
		})
	}
}

// TestLoadConfigRenamedEnvVarsListsAllFindings — несколько устаревших
// переменных сразу: сообщение обязано перечислить ВСЕ найденные старые
// имена, а не только первое встреченное при обходе карты — иначе оператор
// чинит их по одному, по циклу деплоя на переменную.
func TestLoadConfigRenamedEnvVarsListsAllFindings(t *testing.T) {
	names := sortedRenamedOldNames()
	old1, old2 := names[0], names[1]

	_, err := loadConfig(getenvFrom(map[string]string{
		old1: "x",
		old2: "y",
	}), nil)
	if err == nil {
		t.Fatalf("loadConfig: want ошибку на двух устаревших именах, получили nil")
	}
	for _, old := range []string{old1, old2} {
		if !strings.Contains(err.Error(), old) {
			t.Errorf("err = %q, want упоминание %s (найдено больше одного устаревшего имени)", err, old)
		}
	}
}

// TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart — пустое значение
// старого имени не роняет старт: docker-compose штатно прокидывает
// объявленные, но не заданные переменные пустой строкой, и такое значение и
// раньше ничего не применяло бы.
func TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	if _, err := loadConfig(getenvFrom(map[string]string{old: ""}), nil); err != nil {
		t.Errorf("loadConfig с пустым устаревшим %s: %v, want nil (пустое значение легитимно)", old, err)
	}
}

// TestLoadConfigRenamedEnvVarNewNameStillApplies — регрессия: проверка
// устаревших имён не задевает применение НОВОГО имени, пришедшего той же
// волной переименования. Идёт ПОДТЕСТОМ на КАЖДУЮ запись renamedEnvVarNewNameChecks
// (а не берёт одну произвольную пару) — иначе таблица из десятков строк
// защищала бы ровно одно переименование, а остальные были бы никогда
// не вызываемым мёртвым кодом. Имя подтеста — само новое имя переменной:
// упавшая строка сразу называет себя в выводе `go test`.
func TestLoadConfigRenamedEnvVarNewNameStillApplies(t *testing.T) {
	newNames := make([]string, 0, len(renamedEnvVarNewNameChecks))
	for newName := range renamedEnvVarNewNameChecks {
		newNames = append(newNames, newName)
	}
	sort.Strings(newNames)

	for _, newName := range newNames {
		check := renamedEnvVarNewNameChecks[newName]
		t.Run(newName, func(t *testing.T) {
			cfg, err := loadConfig(getenvFrom(map[string]string{newName: check.value}), nil)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := check.get(cfg); got != check.value {
				t.Errorf("%s=%q: соответствующее поле Config = %q, want %q (регрессия применения нового имени)", newName, check.value, got, check.value)
			}
		})
	}
}

func TestLoadConfig_HSTSDefaults(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(nil), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.HSTSEnabled {
		t.Error("HSTSEnabled default = false, want true")
	}
	if cfg.HSTSMaxAgeSeconds != 31536000 {
		t.Errorf("HSTSMaxAgeSeconds default = %d, want 31536000", cfg.HSTSMaxAgeSeconds)
	}
	// includeSubDomains по умолчанию выключен намеренно: инстанс часто живёт
	// на поддомене, и флаг сломал бы соседям HTTPS-требование на весь домен.
	if cfg.HSTSIncludeSubDomains {
		t.Error("HSTSIncludeSubDomains default = true, want false")
	}
	if cfg.HSTSPreload {
		t.Error("HSTSPreload default = true, want false")
	}
}

func TestLoadConfig_HSTSOverrides(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_MAX_AGE_SECONDS":    "600",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HSTSMaxAgeSeconds != 600 || !cfg.HSTSIncludeSubDomains {
		t.Errorf("HSTS = (%d, %v), want (600, true)", cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains)
	}
	// max-age=0 — законное значение (снятие пина), а не ошибка конфига.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_HSTS_MAX_AGE_SECONDS": "0"}), nil)
	if err != nil {
		t.Fatalf("loadConfig max-age=0: %v", err)
	}
	if cfg.HSTSMaxAgeSeconds != 0 {
		t.Errorf("HSTSMaxAgeSeconds = %d, want 0", cfg.HSTSMaxAgeSeconds)
	}
}

func TestLoadConfig_HSTSRejects(t *testing.T) {
	for _, tc := range []struct {
		name          string
		env           map[string]string
		wantErrSubstr string // какое именно правило обязано сработать — три
		// кейса легко перепутать местами или объединить в одну проверку без
		// единого красного теста, если сверять только err != nil
	}{
		{"negative max-age", map[string]string{"GOTCHA_HSTS_MAX_AGE_SECONDS": "-1"},
			"GOTCHA_HSTS_MAX_AGE_SECONDS must be >= 0"},
		{"preload without subdomains", map[string]string{"GOTCHA_HSTS_PRELOAD": "true"},
			"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_INCLUDE_SUBDOMAINS=true"},
		{"preload with short max-age", map[string]string{
			"GOTCHA_HSTS_PRELOAD":            "true",
			"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
			"GOTCHA_HSTS_MAX_AGE_SECONDS":    "600",
		}, "GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_MAX_AGE_SECONDS >= 31536000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(tc.env), nil)
			if err == nil {
				t.Fatal("конфиг принят, ожидался отказ старта")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("ошибка = %q, want подстроку %q (сработало не то правило)", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestLoadConfig_HSTSPreloadAccepted — позитивный кейс к TestLoadConfig_HSTSRejects:
// ровно на границе (max-age год день-в-день) корректная preload-конфигурация
// обязана стартовать, а не просто НЕ отказами — без этого теста замена
// строгого сравнения `< 31536000` на `<= 31536000` в валидации не роняет ни
// один тест (граница в 31536000 никогда не проверяется как «принято»).
func TestLoadConfig_HSTSPreloadAccepted(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_ENABLED":              "true",
		"GOTCHA_HSTS_PRELOAD":              "true",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS":   "true",
		"GOTCHA_HSTS_MAX_AGE_SECONDS":      "31536000",
		"GOTCHA_BASE_URL":                  "https://gotcha.example.com",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}), nil)
	if err != nil {
		t.Fatalf("корректная preload-конфигурация на границе (max-age=31536000) обязана стартовать: %v", err)
	}
	got := web.HSTSHeaderValue(cfg.HSTSEnabled, cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains, cfg.HSTSPreload)
	want := "max-age=31536000; includeSubDomains; preload"
	if got != want {
		t.Errorf("собранный заголовок = %q, want %q", got, want)
	}
}

// TestLoadConfig_HSTSDisabledSkipsPreloadChecks — аварийный откат обязан
// работать: «выключить HSTS, флаги оставить как были» не должен упираться в
// отказ старта, то есть ровно в тот момент, когда сервис и так лежит.
func TestLoadConfig_HSTSDisabledSkipsPreloadChecks(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_HSTS_ENABLED":         "false",
		"GOTCHA_HSTS_PRELOAD":         "true",
		"GOTCHA_HSTS_MAX_AGE_SECONDS": "600",
	}), nil)
	if err != nil {
		t.Fatalf("выключенный HSTS с preload обязан стартовать: %v", err)
	}
	if cfg.HSTSEnabled {
		t.Error("HSTSEnabled = true, want false")
	}
}

// TestLoadConfig_HSTSWarnings — две ситуации, о которых оператор иначе не
// узнает: заголовок не уйдёт (BASE_URL не https) и настройки игнорируются
// (HSTS выключен, а флаги заданы).
func TestLoadConfig_HSTSWarnings(t *testing.T) {
	capture := func(t *testing.T, env map[string]string) []slog.Record {
		t.Helper()
		var records []slog.Record
		prev := slog.Default()
		slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
		defer slog.SetDefault(prev)
		if _, err := loadConfig(getenvFrom(env), nil); err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		return records
	}
	hasWarn := func(records []slog.Record, substr string) bool {
		for _, r := range records {
			if r.Level == slog.LevelWarn && strings.Contains(r.Message, substr) {
				return true
			}
		}
		return false
	}

	// BASE_URL по дефолту http://localhost:8080 — заголовка не будет.
	if got := capture(t, map[string]string{"GOTCHA_HSTS_ENABLED": "true"}); !hasWarn(got, "GOTCHA_BASE_URL") {
		t.Error("нет предупреждения о том, что HSTS включён при не-https GOTCHA_BASE_URL")
	}
	// Выключенный HSTS с заданными флагами — они игнорируются.
	if got := capture(t, map[string]string{
		"GOTCHA_HSTS_ENABLED":            "false",
		"GOTCHA_HSTS_INCLUDE_SUBDOMAINS": "true",
	}); !hasWarn(got, "GOTCHA_HSTS_INCLUDE_SUBDOMAINS") {
		t.Error("нет предупреждения о том, что настройка HSTS игнорируется при выключенном HSTS")
	}
	// Обратный случай: ничего лишнего не задано — предупреждения об игнорировании нет.
	if got := capture(t, map[string]string{
		"GOTCHA_HSTS_ENABLED":              "false",
		"GOTCHA_BASE_URL":                  "https://gotcha.example",
		"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "1",
	}); hasWarn(got, "ignored") {
		t.Error("предупреждение об игнорировании выдано, хотя ни одна настройка HSTS не задана")
	}
}

// TestLoadConfig_HSTSWarningSkippedOutsideWebModes — в ingest/uptime/probe
// web.Handler вообще не строится (main.go поднимает его только под
// cfg.Mode == "web" || cfg.Mode == "all"), значит заголовок Strict-Transport-
// Security структурно невозможен: предупреждение про не-https GOTCHA_BASE_URL
// не о чем предупреждать и только шумит на каждом старте приёмного узла или
// dev-стенда. GOTCHA_BASE_URL по умолчанию http://localhost:8080 — то же
// значение, что в TestLoadConfig_HSTSWarnings роняет предупреждение в
// дефолтном режиме all.
func TestLoadConfig_HSTSWarningSkippedOutsideWebModes(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prev)

	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_HSTS_ENABLED": "true"}),
		[]string{"--mode", "ingest"}); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "GOTCHA_BASE_URL") {
			t.Errorf("предупреждение про GOTCHA_BASE_URL выдано в режиме ingest, где web.Handler не строится: %q", r.Message)
		}
	}
}

// TestLoadConfig_StringEnvTrimmed — обычные str()-переменные обрезаются по
// краям: раньше "GOTCHA_LOCALE=" ru"" падало с текстом `got " ru"`, хотя
// оператор явно указал допустимую локаль, просто с лишним пробелом.
func TestLoadConfig_StringEnvTrimmed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_LOCALE":              " ru",
		"GOTCHA_UPTIME_LOCAL_REGION": " eu-fra \t",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Locale != "ru" {
		t.Errorf("Locale = %q, want %q (leading space must be trimmed)", cfg.Locale, "ru")
	}
	if cfg.LocalRegion != "eu-fra" {
		t.Errorf("LocalRegion = %q, want %q", cfg.LocalRegion, "eu-fra")
	}
}

// TestLoadConfig_WhitespaceOnlyStringFallsBackToDefault — строка из одних
// пробелов у обычной str()-переменной (не входит в strGuarded-список) — то же
// самое, что переменная не задана: тихий откат на def, без ошибки старта.
func TestLoadConfig_WhitespaceOnlyStringFallsBackToDefault(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_UPTIME_LOCAL_REGION": "   "}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.LocalRegion != "local" {
		t.Errorf("LocalRegion = %q, want default %q (whitespace-only counts as unset)", cfg.LocalRegion, "local")
	}
}

// TestLoadConfig_SecretKeyTrimmed — "abc ", " abc" и "abc" обязаны дать один и
// тот же мастер-ключ. Раньше хвостовой/ведущий пробел молча входил в ключ:
// оператор, «поправив» файл и убрав «лишний» пробел, тихо получал другой
// ключ и терял всё, что было зашифровано под старым (секреты каналов, SSO).
// BaseURL по умолчанию localhost — SEC-C1 (дефолтный/короткий ключ) тут не
// участвует, тест только про сам тримминг.
func TestLoadConfig_SecretKeyTrimmed(t *testing.T) {
	for _, raw := range []string{"abc", "abc ", " abc", "\tabc\n"} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_SECRET_KEY": raw}), nil)
		if err != nil {
			t.Fatalf("GOTCHA_SECRET_KEY=%q: loadConfig: %v", raw, err)
		}
		if cfg.SecretKey != "abc" {
			t.Errorf("GOTCHA_SECRET_KEY=%q: SecretKey = %q, want %q", raw, cfg.SecretKey, "abc")
		}
	}
}

// TestLoadConfig_BlankGuardedStringsRejected — GOTCHA_SECRET_KEY/GOTCHA_PG_DSN/
// GOTCHA_CH_DSN обязаны отказать старт на пробельном (но непустом) значении, а
// не тихо откатиться на дефолт: для секрета дефолт — публично известный
// insecure-dev-secret, для DSN — localhost вместо прод-базы, которую оператор
// явно указывал.
func TestLoadConfig_BlankGuardedStringsRejected(t *testing.T) {
	for _, key := range []string{"GOTCHA_SECRET_KEY", "GOTCHA_PG_DSN", "GOTCHA_CH_DSN"} {
		_, err := loadConfig(getenvFrom(map[string]string{key: "   "}), nil)
		if err == nil {
			t.Errorf("%s=\"   \": ждали ошибку старта, получили nil", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s=\"   \": ошибка не называет переменную: %v", key, err)
		}
	}
}

// TestLoadConfig_GuardedDSNsTrimmed — GOTCHA_PG_DSN/GOTCHA_CH_DSN с пробелами
// по краям читаются как непустое явное значение (обрезанное), а не как
// «не задано».
func TestLoadConfig_GuardedDSNsTrimmed(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_PG_DSN": " postgres://u:p@pg:5432/g ",
		"GOTCHA_CH_DSN": " clickhouse://ch:9000/g\t",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.PostgresDSN != "postgres://u:p@pg:5432/g" {
		t.Errorf("PostgresDSN = %q", cfg.PostgresDSN)
	}
	if cfg.ClickHouseDSN != "clickhouse://ch:9000/g" {
		t.Errorf("ClickHouseDSN = %q", cfg.ClickHouseDSN)
	}
}

// TestLoadConfig_ProbeCredsWhitespaceOnlyRejected — ключ и URL пробы обязаны
// отказать старт на пробельном значении в режиме --mode=probe. Они читаются
// обычным str() (def == ""), поэтому пробельное значение уже трактуется как
// «не задано» — и в это же «не задано» упирается существующая обязательность
// пробы: отдельный strGuarded для них не нужен, но контракт должен быть
// закрыт тестом, а не молчаливым допущением.
func TestLoadConfig_ProbeCredsWhitespaceOnlyRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"пробельный токен": {
			"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
			"GOTCHA_PROBE_KEY":        "   ",
		},
		"пробельный server url": {
			"GOTCHA_PROBE_SERVER_URL": "   ",
			"GOTCHA_PROBE_KEY":        "ptok",
		},
	}
	for name, env := range cases {
		if _, err := loadConfig(getenvFrom(env), []string{"--mode=probe"}); err == nil {
			t.Errorf("%s: ждали отказ старта в --mode=probe, получили nil", name)
		}
	}
}

// TestLoadConfigExportValidatedAtStartup — экспортная четвёрка
// (GOTCHA_EXPORT_RETENTION_HOURS/_MAX_ROWS/_MAX_BYTES/_DISK_BUDGET_BYTES)
// проверяется export.Config.Validate() уже на старте, а не только в первом
// тике воркера (internal/export/worker.go): раньше Run() глотал её ошибку
// как slog.Warn на каждом тике, процесс стартовал с виду здоровым, раздел
// «Выгрузки» был виден в UI, а заявки копились в очереди навсегда. Текст
// ошибки обязан называть переменную окружения, а не поле структуры Go
// (MaxRows и т.п.) — оно оператору ни о чём не говорит.
func TestLoadConfigExportValidatedAtStartup(t *testing.T) {
	cases := []struct{ key, value string }{
		{"GOTCHA_EXPORT_MAX_ROWS", "0"},
		{"GOTCHA_EXPORT_MAX_BYTES", "0"},
		{"GOTCHA_EXPORT_DISK_BUDGET_BYTES", "0"},
		{"GOTCHA_EXPORT_RETENTION_HOURS", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			_, err := loadConfig(getenvFrom(map[string]string{tc.key: tc.value}), nil)
			if err == nil {
				t.Fatalf("%s=%s: want error, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s=%s: error %q does not name the variable", tc.key, tc.value, err)
			}
			for _, field := range []string{"MaxRows", "MaxBytes", "DiskBudget"} {
				if strings.Contains(err.Error(), field) {
					t.Errorf("%s=%s: error %q still names the Go struct field %q instead of the env var", tc.key, tc.value, err, field)
				}
			}
		})
	}
}

// TestLoadConfigAllowInsecureSecretGarbageParsedRegardlessOfKeyStrength —
// GOTCHA_SECRET_KEY_ALLOW_INSECURE разбирается ДО обеих secret-проверок ниже, а
// не inline через boolEnv() в их && условиях. Раньше при СИЛЬНОМ кастомном
// ключе короткое замыкание останавливалось на состоянии самого ключа
// (== devSecretKey / длина < 32) раньше, чем доходило до boolEnv() —
// мусорное значение переменной («ture») никогда не разбиралось и не
// попадало в errs: fail-fast, зависящий от порядка вычисления выражения, а
// не от того, что оператор реально написал в .env.
func TestLoadConfigAllowInsecureSecretGarbageParsedRegardlessOfKeyStrength(t *testing.T) {
	strong := strings.Repeat("a", 32) // ровно 32 байта — сильный ключ
	short := "0123456789abcdef"       // 16 байт — слабый ключ

	t.Run("нормальный ключ + мусор", func(t *testing.T) {
		_, err := loadConfig(getenvFrom(map[string]string{
			"GOTCHA_BASE_URL":                  "https://gotcha.example",
			"GOTCHA_SECRET_KEY":                strong,
			"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "ture",
		}), []string{"--mode=web"})
		if err == nil {
			t.Fatal("сильный ключ + мусор в GOTCHA_SECRET_KEY_ALLOW_INSECURE: want error, got nil (раньше значение не разбиралось вовсе)")
		}
		if !strings.Contains(err.Error(), "GOTCHA_SECRET_KEY_ALLOW_INSECURE") || !strings.Contains(err.Error(), "invalid boolean") {
			t.Errorf("error = %q, want it to name GOTCHA_SECRET_KEY_ALLOW_INSECURE and say 'invalid boolean'", err)
		}
	})

	t.Run("слабый ключ + мусор", func(t *testing.T) {
		_, err := loadConfig(getenvFrom(map[string]string{
			"GOTCHA_BASE_URL":                  "https://gotcha.example",
			"GOTCHA_SECRET_KEY":                short,
			"GOTCHA_SECRET_KEY_ALLOW_INSECURE": "ture",
		}), []string{"--mode=web"})
		if err == nil {
			t.Fatal("слабый ключ + мусор в GOTCHA_SECRET_KEY_ALLOW_INSECURE: want error, got nil")
		}
	})
}

// TestLoadConfig_EnumsCaseInsensitive — E3 задача 5: GOTCHA_EDITION,
// GOTCHA_REGISTRATION_MODE и GOTCHA_LOCALE сравниваются после trim+lower. Раньше
// "EDITION=OSS" (капс — принятая форма записи значений env самим же
// оператором) ронял старт с "must be oss or saas", хотя это ровно
// документированное значение, только в другом регистре. Расширение
// принимаемого, не сужение: уже принятые строчные значения продолжают
// работать (см. TestLoadConfig_Edition/_Registration/_Locale).
func TestLoadConfig_EnumsCaseInsensitive(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION":           "OSS",
		"GOTCHA_REGISTRATION_MODE": "OPEN",
		"GOTCHA_LOCALE":            "EN",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Edition != "oss" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "oss")
	}
	if cfg.RegistrationMode != "open" {
		t.Errorf("RegistrationMode = %q, want %q", cfg.RegistrationMode, "open")
	}
	if cfg.Locale != "en" {
		t.Errorf("Locale = %q, want %q", cfg.Locale, "en")
	}

	// Смешанный регистр и лишние пробелы по краям — тоже принимается.
	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_EDITION": " SaaS ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig mixed case: %v", err)
	}
	if cfg.Edition != "saas" {
		t.Errorf("Edition = %q, want %q", cfg.Edition, "saas")
	}
	if cfg.DefaultEventQuota != 1_000_000 {
		t.Errorf("DefaultEventQuota (SaaS с пробелами) = %d, want 1000000 — редакция должна распознаться", cfg.DefaultEventQuota)
	}

	// По-прежнему мусор — ошибка (регистр не спасает опечатку).
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_EDITION": "BOGUS"}), nil); err == nil {
		t.Error("BOGUS GOTCHA_EDITION must fail regardless of case")
	}
}

// TestLoadConfig_LogLevelFormatTrimmedAndLowered — cfg.LogLevel/cfg.LogFormat
// приходят из GOTCHA_LOGGING_LEVEL/GOTCHA_LOGGING_FORMAT уже trim+lower (loadConfig
// сам их не валидирует — это делает setupLogging в main.go, см.
// TestSetupLoggingWarningAliasSetsWarnLevel в wiring_test.go), поэтому
// "WARNING" в любом регистре обязан дойти до setupLogging как "warning".
func TestLoadConfig_LogLevelFormatTrimmedAndLowered(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_LOGGING_LEVEL":  " WARNING ",
		"GOTCHA_LOGGING_FORMAT": " JSON ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.LogLevel != "warning" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "warning")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
}

// TestTrustedRecipientsWhitespaceAndEmptyElements — бриф задачи 5, дословный
// кейс: пробелы по краям каждого элемента и пустой элемент от двойной
// запятой не портят список из двух реальных доменов.
func TestTrustedRecipientsWhitespaceAndEmptyElements(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{
		"GOTCHA_TRUSTED_RECIPIENTS": " a.example , ,b.example ",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"a.example", "b.example"}
	if len(cfg.TrustedRecipients) != len(want) {
		t.Fatalf("TrustedRecipients = %v, want %v", cfg.TrustedRecipients, want)
	}
	for i, w := range want {
		if cfg.TrustedRecipients[i] != w {
			t.Errorf("TrustedRecipients[%d] = %q, want %q", i, cfg.TrustedRecipients[i], w)
		}
	}
}
