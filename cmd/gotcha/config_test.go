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
	if cfg.DefaultEventQuota != 1000000 {
		t.Errorf("DefaultEventQuota = %d, want 1000000", cfg.DefaultEventQuota)
	}
	if cfg.MaxEventBytes != 1048576 {
		t.Errorf("MaxEventBytes = %d, want 1048576", cfg.MaxEventBytes)
	}
	if cfg.SecretKey != "insecure-dev-secret" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
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
		"GOTCHA_DEFAULT_EVENT_QUOTA": "50000",
		"GOTCHA_MAX_EVENT_BYTES":     "2097152",
		"GOTCHA_SECRET_KEY":          "prod-secret",
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
	if cfg.SecretKey != "prod-secret" {
		t.Errorf("SecretKey = %q", cfg.SecretKey)
	}
}

func TestLoadConfigInvalidMode(t *testing.T) {
	if _, err := loadConfig(getenvFrom(nil), []string{"--mode", "banana"}); err == nil {
		t.Fatal("want error for invalid mode, got nil")
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
