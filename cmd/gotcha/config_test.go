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
}

func TestLoadConfigOverrides(t *testing.T) {
	env := map[string]string{
		"GOTCHA_ADDR":                ":9090",
		"GOTCHA_BASE_URL":            "https://errors.example.com",
		"GOTCHA_PG_DSN":              "postgres://u:p@pg:5432/g",
		"GOTCHA_CH_DSN":              "clickhouse://ch:9000/g",
		"GOTCHA_SMTP_HOST":           "smtp.example.com",
		"GOTCHA_SMTP_PORT":           "465",
		"GOTCHA_SMTP_USER":           "mailer",
		"GOTCHA_SMTP_PASSWORD":       "s3cret",
		"GOTCHA_SMTP_FROM":           "gotcha@example.com",
		"GOTCHA_RETENTION_DAYS":      "30",
		"GOTCHA_SPAN_RETENTION_DAYS": "7",
		"GOTCHA_DEFAULT_EVENT_QUOTA": "50000",
		"GOTCHA_MAX_EVENT_BYTES":     "2097152",
		"GOTCHA_SECRET_KEY":          "prod-secret",
		"GOTCHA_UPTIME_CONCURRENCY":  "10",
		"GOTCHA_LOCAL_REGION":        "eu-fra",
		"GOTCHA_PROBE_TOKEN":         "ptok",
		"GOTCHA_SERVER_URL":          "https://gotcha.example.com",
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
	if cfg.SecretKey != "prod-secret" {
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

func TestLoadConfigInvalidMode(t *testing.T) {
	if _, err := loadConfig(getenvFrom(nil), []string{"--mode", "banana"}); err == nil {
		t.Fatal("want error for invalid mode, got nil")
	}
}

func TestLoadConfigAcceptsUptimeAndProbeModes(t *testing.T) {
	// probe без GOTCHA_SERVER_URL/GOTCHA_PROBE_TOKEN не запускается (см.
	// TestLoadConfigProbeModeRequiresServerURLAndToken), поэтому здесь они
	// заданы для обоих режимов — проверяется только разбор --mode.
	env := map[string]string{
		"GOTCHA_SERVER_URL":  "https://gotcha.example.com",
		"GOTCHA_PROBE_TOKEN": "probe-token",
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
			"GOTCHA_SERVER_URL":  serverURL,
			"GOTCHA_PROBE_TOKEN": "probe-token",
		}
		if _, err := loadConfig(getenvFrom(env), []string{"--mode", "probe"}); err == nil {
			t.Errorf("GOTCHA_SERVER_URL=%q: want error, got nil", serverURL)
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
	env := map[string]string{"GOTCHA_RETENTION_DAYS": "ninety"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("want error for non-numeric GOTCHA_RETENTION_DAYS, got nil")
	}
}

func TestLoadConfigNonPositiveRetention(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		env := map[string]string{"GOTCHA_RETENTION_DAYS": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Fatalf("GOTCHA_RETENTION_DAYS=%q: want error, got nil", v)
		}
	}
}

func TestLoadConfigNonPositiveSpanRetention(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		env := map[string]string{"GOTCHA_SPAN_RETENTION_DAYS": v}
		if _, err := loadConfig(getenvFrom(env), nil); err == nil {
			t.Fatalf("GOTCHA_SPAN_RETENTION_DAYS=%q: want error, got nil", v)
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
		{"no token", map[string]string{"GOTCHA_SERVER_URL": "https://gotcha.example.com"}},
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
		"GOTCHA_SERVER_URL":  "https://gotcha.example.com",
		"GOTCHA_PROBE_TOKEN": "probe-token",
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

// Остальные режимы GOTCHA_SERVER_URL/GOTCHA_PROBE_TOKEN не требуют.
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
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROFILE_RETENTION_DAYS": "0"}), nil); err == nil {
		t.Error("zero profile retention must fail")
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
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_OUTBOX_RETENTION_DAYS": "0"}), nil); err == nil {
		t.Error("zero outbox retention must fail")
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

func TestLoadConfig_IngestModeDoesNotRequireSecret(t *testing.T) {
	env := map[string]string{"GOTCHA_BASE_URL": "https://gotcha.example.com"}
	getenv := func(k string) string { return env[k] }
	if _, err := loadConfig(getenv, []string{"--mode=ingest"}); err != nil {
		t.Fatalf("ingest mode has no oauth cookie, must not require secret, got: %v", err)
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
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_PROFILE_EVAL_INTERVAL": "0"}), nil); err == nil {
		t.Error("zero profile eval interval must fail")
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
	if cfg.DefaultTransactionQuota != 0 || cfg.DefaultMetricQuota != 0 || cfg.DefaultProfileQuota != 0 {
		t.Errorf("oss quotas not all 0: tx=%d metric=%d profile=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota)
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
	if cfg.DefaultTransactionQuota != 1_000_000 || cfg.DefaultMetricQuota != 1_000_000 || cfg.DefaultProfileQuota != 1_000_000 {
		t.Errorf("saas quotas not all 1000000: tx=%d metric=%d profile=%d",
			cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota)
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

	// Явные env-переопределения всех четырёх дефолтов.
	cfg, err = loadConfig(getenvFrom(map[string]string{
		"GOTCHA_DEFAULT_EVENT_QUOTA":       "10",
		"GOTCHA_DEFAULT_TRANSACTION_QUOTA": "20",
		"GOTCHA_DEFAULT_METRIC_QUOTA":      "30",
		"GOTCHA_DEFAULT_PROFILE_QUOTA":     "40",
	}), nil)
	if err != nil {
		t.Fatalf("loadConfig overrides: %v", err)
	}
	if cfg.DefaultEventQuota != 10 || cfg.DefaultTransactionQuota != 20 ||
		cfg.DefaultMetricQuota != 30 || cfg.DefaultProfileQuota != 40 {
		t.Errorf("quota overrides failed: event=%d tx=%d metric=%d profile=%d",
			cfg.DefaultEventQuota, cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota)
	}

	// Отрицательная квота — ошибка (0 разрешён, <0 нет).
	if _, err := loadConfig(getenvFrom(map[string]string{"GOTCHA_DEFAULT_METRIC_QUOTA": "-1"}), nil); err == nil {
		t.Error("negative GOTCHA_DEFAULT_METRIC_QUOTA must fail")
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

	// Пользовательский CSV-список ключей.
	cfg, err = loadConfig(getenvFrom(map[string]string{"GOTCHA_SCRUB_KEYS": "a,b"}), nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.ScrubKeys) != 2 || cfg.ScrubKeys[0] != "a" || cfg.ScrubKeys[1] != "b" {
		t.Errorf("ScrubKeys = %v, want [a b]", cfg.ScrubKeys)
	}
}
