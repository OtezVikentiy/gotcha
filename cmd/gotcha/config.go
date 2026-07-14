package main

import (
	"flag"
	"fmt"
	"strconv"
)

// Config собирается из env (префикс GOTCHA_) и флагов командной строки.
type Config struct {
	Mode              string // ingest | web | all
	Addr              string
	BaseURL           string
	PostgresDSN       string
	ClickHouseDSN     string
	SMTPHost          string
	SMTPPort          int
	SMTPUser          string
	SMTPPassword      string
	SMTPFrom          string
	RetentionDays     int
	DefaultEventQuota int64
	MaxEventBytes     int64
	SecretKey         string
}

func loadConfig(getenv func(string) string, args []string) (Config, error) {
	fs := flag.NewFlagSet("gotcha", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process role: ingest | web | all")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *mode != "ingest" && *mode != "web" && *mode != "all" {
		return Config{}, fmt.Errorf("invalid --mode %q: want ingest, web or all", *mode)
	}

	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	var errs []error
	num := func(key string, def int64) int64 {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return n
	}

	cfg := Config{
		Mode:              *mode,
		Addr:              str("GOTCHA_ADDR", ":8080"),
		BaseURL:           str("GOTCHA_BASE_URL", "http://localhost:8080"),
		PostgresDSN:       str("GOTCHA_PG_DSN", "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable"),
		ClickHouseDSN:     str("GOTCHA_CH_DSN", "clickhouse://localhost:9000/gotcha"),
		SMTPHost:          str("GOTCHA_SMTP_HOST", ""),
		SMTPPort:          int(num("GOTCHA_SMTP_PORT", 587)),
		SMTPUser:          str("GOTCHA_SMTP_USER", ""),
		SMTPPassword:      str("GOTCHA_SMTP_PASSWORD", ""),
		SMTPFrom:          str("GOTCHA_SMTP_FROM", ""),
		RetentionDays:     int(num("GOTCHA_RETENTION_DAYS", 90)),
		DefaultEventQuota: num("GOTCHA_DEFAULT_EVENT_QUOTA", 1_000_000),
		MaxEventBytes:     num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		SecretKey:         str("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
	}
	if len(errs) > 0 {
		return Config{}, errs[0]
	}

	if cfg.RetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_RETENTION_DAYS must be >= 1, got %d", cfg.RetentionDays)
	}
	if cfg.DefaultEventQuota < 1 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_EVENT_QUOTA must be >= 1, got %d", cfg.DefaultEventQuota)
	}
	if cfg.MaxEventBytes < 1 {
		return Config{}, fmt.Errorf("GOTCHA_MAX_EVENT_BYTES must be >= 1, got %d", cfg.MaxEventBytes)
	}

	return cfg, nil
}
