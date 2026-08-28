package main

import (
	"strings"
	"testing"
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
		"GOTCHA_EXPORT_TTL_HOURS":         "24",
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
		"GOTCHA_ADDR":                 ":9090",
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
		"GOTCHA_LOCAL_REGION":         "eu-fra",
		"GOTCHA_PROBE_TOKEN":          "ptok",
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
		env := map[string]string{"GOTCHA_BASE_URL": tc.in, "GOTCHA_ALLOW_INSECURE_SECRET": "1"}
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
	// probe без GOTCHA_PROBE_SERVER_URL/GOTCHA_PROBE_TOKEN не запускается (см.
	// TestLoadConfigProbeModeRequiresServerURLAndToken), поэтому здесь они
	// заданы для обоих режимов — проверяется только разбор --mode.
	env := map[string]string{
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example.com",
		"GOTCHA_PROBE_TOKEN":      "probe-token",
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
			"GOTCHA_PROBE_TOKEN":      "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_PROBE_SERVER_URL=%q: want error, got nil", serverURL)
		}
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
		{"no server url", map[string]string{"GOTCHA_PROBE_TOKEN": "t"}},
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
		"GOTCHA_PROBE_TOKEN":      "probe-token",
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

// Остальные режимы GOTCHA_PROBE_SERVER_URL/GOTCHA_PROBE_TOKEN не требуют.
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
		"GOTCHA_OIDC_NAME":            "Corp SSO",
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
		"GOTCHA_BASE_URL":              "https://gotcha.example.com",
		"GOTCHA_ALLOW_INSECURE_SECRET": "1",
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
		"GOTCHA_PROBE_TOKEN":      "ptok",
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
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION": mode}), nil)
		if err != nil {
			t.Fatalf("loadConfig %q: %v", mode, err)
		}
		if cfg.RegistrationMode != mode {
			t.Errorf("RegistrationMode = %q, want %q", cfg.RegistrationMode, mode)
		}
	}
	// Мусорное значение — ошибка.
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_REGISTRATION": "bogus"}), nil); err == nil {
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
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_KEYS": "a,b"}), nil)
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
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_KEYS": ",,"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasAll(cfg.ScrubKeys, defaultScrubKeys()) {
		t.Errorf("ScrubKeys = %v при GOTCHA_SCRUB_KEYS=\",,\" — denylist обнулён", cfg.ScrubKeys)
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
// Иначе `--migrate-only` вместе с GOTCHA_AUTO_MIGRATE=false — а это ровно та
// конфигурация, для которой флаг и нужен, — только проверил бы схему и вышел,
// ничего не применив.
func TestMigrateOnlyImpliesAutoMigrate(t *testing.T) {
	cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_AUTO_MIGRATE": "false"}), []string{"--migrate-only"})
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
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_TOKEN": "t",
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
		"GOTCHA_PROBE_SERVER_URL": "https://gotcha.example", "GOTCHA_PROBE_TOKEN": "t",
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
		{"GOTCHA_PURGE_RECONCILE_HOURS", "-1"},
		{"GOTCHA_NOTIFY_CONCURRENCY", "0"},
		{"GOTCHA_SLO_EVAL_INTERVAL_SECONDS", "0"},
		{"GOTCHA_DEPENDENCY_SETTLE_SECONDS", "-1"},
		{"GOTCHA_DEFAULT_TRANSACTION_QUOTA", "-1"},
		{"GOTCHA_DEFAULT_PROFILE_QUOTA", "-1"},
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
// «8MiB» разбирается в 0, а 0 для GOTCHA_MAX_BUFFER_BYTES означает «выведи
// потолок сам» (см. effectiveMaxBufferBytes), то есть опечатка тихо меняла бы
// смысл настройки вместо отказа на старте.
func TestLoadConfigRejectsNonNumericInt64(t *testing.T) {
	_, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_MAX_BUFFER_BYTES": "8MiB"}), nil)
	if err == nil {
		t.Fatal("GOTCHA_MAX_BUFFER_BYTES=8MiB: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_MAX_BUFFER_BYTES") {
		t.Errorf("error = %q, want it to name GOTCHA_MAX_BUFFER_BYTES", err)
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
	env := map[string]string{"GOTCHA_SCRUB_ALLOW_KEYS": " Order_ID , ,USER_ID"}
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
		"GOTCHA_BASE_URL":              "https://gotcha.example",
		"GOTCHA_SECRET_KEY":            short,
		"GOTCHA_ALLOW_INSECURE_SECRET": "1",
	}
	if _, err := loadConfig(getenvFrom(withEscape), []string{"--mode=web"}); err != nil {
		t.Fatalf("GOTCHA_ALLOW_INSECURE_SECRET=1 должен разрешать короткий ключ: %v", err)
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
		"GOTCHA_PROBE_TOKEN":      "tok",
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
		"GOTCHA_PROBE_TOKEN":      "tok",
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
		{"1", true}, {"true", true}, {"YES", true},
		{"0", false}, {"false", false}, {"nonsense", false},
	} {
		cfg, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_RUN_EVALUATORS": tc.value}), nil)
		if err != nil {
			t.Fatalf("GOTCHA_RUN_EVALUATORS=%q: loadConfig: %v", tc.value, err)
		}
		if cfg.RunEvaluators == nil {
			t.Errorf("GOTCHA_RUN_EVALUATORS=%q: RunEvaluators = nil, want заданное значение", tc.value)
			continue
		}
		if *cfg.RunEvaluators != tc.want {
			t.Errorf("GOTCHA_RUN_EVALUATORS=%q: RunEvaluators = %v, want %v", tc.value, *cfg.RunEvaluators, tc.want)
		}
	}
}
