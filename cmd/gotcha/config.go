package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
)

// Config собирается из env (префикс GOTCHA_) и флагов командной строки.
type Config struct {
	Mode              string // ingest | web | uptime | probe | all
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
	SpanRetentionDays int
	DefaultEventQuota int64
	MaxEventBytes     int64
	SecretKey         string

	// UptimeConcurrency — сколько проверок uptime.Runner выполняет
	// одновременно (режимы uptime|all).
	UptimeConcurrency int
	// LocalRegion — имя встроенного региона локальной пробы (см.
	// uptime.DefaultRegion), используется uptime.Runner.
	LocalRegion string
	// ProbeToken/ServerURL — учётные данные выносной пробы (--mode=probe):
	// база центра и Bearer-токен пробы. В этом режиме обязательны — больше
	// пробе знать нечего (ни PG, ни CH она не открывает).
	ProbeToken string
	ServerURL  string
}

var validModes = map[string]bool{
	"ingest": true, "web": true, "uptime": true, "probe": true, "all": true,
}

func loadConfig(getenv func(string) string, args []string) (Config, error) {
	fs := flag.NewFlagSet("gotcha", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process role: ingest | web | uptime | probe | all")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if !validModes[*mode] {
		return Config{}, fmt.Errorf("invalid --mode %q: want ingest, web, uptime, probe or all", *mode)
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
		SpanRetentionDays: int(num("GOTCHA_SPAN_RETENTION_DAYS", 30)),
		DefaultEventQuota: num("GOTCHA_DEFAULT_EVENT_QUOTA", 1_000_000),
		MaxEventBytes:     num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		SecretKey:         str("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
		UptimeConcurrency: int(num("GOTCHA_UPTIME_CONCURRENCY", 50)),
		LocalRegion:       str("GOTCHA_LOCAL_REGION", "local"),
		ProbeToken:        str("GOTCHA_PROBE_TOKEN", ""),
		ServerURL:         str("GOTCHA_SERVER_URL", ""),
	}
	if len(errs) > 0 {
		return Config{}, errs[0]
	}

	if cfg.RetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_RETENTION_DAYS must be >= 1, got %d", cfg.RetentionDays)
	}
	if cfg.SpanRetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_SPAN_RETENTION_DAYS must be >= 1, got %d", cfg.SpanRetentionDays)
	}
	if cfg.DefaultEventQuota < 1 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_EVENT_QUOTA must be >= 1, got %d", cfg.DefaultEventQuota)
	}
	if cfg.MaxEventBytes < 1 {
		return Config{}, fmt.Errorf("GOTCHA_MAX_EVENT_BYTES must be >= 1, got %d", cfg.MaxEventBytes)
	}
	if cfg.UptimeConcurrency < 1 {
		return Config{}, fmt.Errorf("GOTCHA_UPTIME_CONCURRENCY must be >= 1, got %d", cfg.UptimeConcurrency)
	}
	if cfg.Mode == "probe" {
		if cfg.ServerURL == "" {
			return Config{}, fmt.Errorf("GOTCHA_SERVER_URL is required with --mode=probe")
		}
		// Схему и хост проверяем на старте: без них каждый тик пробы (раз в
		// секунду, вечно) падал бы с "unsupported protocol scheme" — тихий
		// бесконечный цикл ошибок вместо внятного отказа при запуске.
		u, err := url.Parse(cfg.ServerURL)
		if err != nil {
			return Config{}, fmt.Errorf("GOTCHA_SERVER_URL: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("GOTCHA_SERVER_URL must be an absolute http(s) url, got %q", cfg.ServerURL)
		}
		if cfg.ProbeToken == "" {
			return Config{}, fmt.Errorf("GOTCHA_PROBE_TOKEN is required with --mode=probe")
		}
	}

	return cfg, nil
}
