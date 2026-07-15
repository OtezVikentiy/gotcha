package main

import "testing"

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
	if cfg.DefaultEventQuota != 1000000 {
		t.Errorf("DefaultEventQuota = %d, want 1000000", cfg.DefaultEventQuota)
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
	env := map[string]string{"GOTCHA_DEFAULT_EVENT_QUOTA": "0"}
	if _, err := loadConfig(getenvFrom(env), nil); err == nil {
		t.Fatal("GOTCHA_DEFAULT_EVENT_QUOTA=0: want error, got nil")
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
