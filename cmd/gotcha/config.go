package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Config собирается из env (префикс GOTCHA_) и флагов командной строки.
type Config struct {
	Mode                 string // ingest | web | uptime | probe | all
	Addr                 string
	BaseURL              string
	PostgresDSN          string
	ClickHouseDSN        string
	SMTPHost             string
	SMTPPort             int
	SMTPUser             string
	SMTPPassword         string
	SMTPFrom             string
	RetentionDays        int
	SpanRetentionDays    int
	MetricRetentionDays  int
	ProfileRetentionDays int
	DefaultEventQuota    int64
	MaxEventBytes        int64
	MetricQuota          int64
	MetricEvalInterval   int
	ProfileQuota         int64
	OutboxRetentionDays  int
	SecretKey            string

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

	// OAuth/social login (этап 5). Каждый провайдер включается независимо;
	// включённый без обязательных секретов → отказ на старте. Секреты живут
	// только в памяти процесса.
	OIDCEnabled        bool
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCScopes         string
	OIDCName           string
	YandexEnabled      bool
	YandexClientID     string
	YandexClientSecret string
	VKEnabled          bool
	VKClientID         string
	VKClientSecret     string
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

	boolEnv := func(key string) bool {
		v := strings.ToLower(strings.TrimSpace(getenv(key)))
		return v == "1" || v == "true" || v == "yes" || v == "on"
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
		Mode:                 *mode,
		Addr:                 str("GOTCHA_ADDR", ":8080"),
		BaseURL:              str("GOTCHA_BASE_URL", "http://localhost:8080"),
		PostgresDSN:          str("GOTCHA_PG_DSN", "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable"),
		ClickHouseDSN:        str("GOTCHA_CH_DSN", "clickhouse://localhost:9000/gotcha"),
		SMTPHost:             str("GOTCHA_SMTP_HOST", ""),
		SMTPPort:             int(num("GOTCHA_SMTP_PORT", 587)),
		SMTPUser:             str("GOTCHA_SMTP_USER", ""),
		SMTPPassword:         str("GOTCHA_SMTP_PASSWORD", ""),
		SMTPFrom:             str("GOTCHA_SMTP_FROM", ""),
		RetentionDays:        int(num("GOTCHA_RETENTION_DAYS", 90)),
		SpanRetentionDays:    int(num("GOTCHA_SPAN_RETENTION_DAYS", 30)),
		MetricRetentionDays:  int(num("GOTCHA_METRIC_RETENTION_DAYS", 30)),
		ProfileRetentionDays: int(num("GOTCHA_PROFILE_RETENTION_DAYS", 7)),
		DefaultEventQuota:    num("GOTCHA_DEFAULT_EVENT_QUOTA", 1_000_000),
		MaxEventBytes:        num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		MetricQuota:          num("GOTCHA_METRIC_QUOTA", 1_000_000),
		MetricEvalInterval:   int(num("GOTCHA_METRIC_EVAL_INTERVAL", 60)),
		ProfileQuota:         num("GOTCHA_PROFILE_QUOTA", 1_000_000),
		OutboxRetentionDays:  int(num("GOTCHA_OUTBOX_RETENTION_DAYS", 7)),
		SecretKey:            str("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
		UptimeConcurrency:    int(num("GOTCHA_UPTIME_CONCURRENCY", 50)),
		LocalRegion:          str("GOTCHA_LOCAL_REGION", "local"),
		ProbeToken:           str("GOTCHA_PROBE_TOKEN", ""),
		ServerURL:            str("GOTCHA_SERVER_URL", ""),
	}
	cfg.OIDCEnabled = boolEnv("GOTCHA_OIDC_ENABLED")
	cfg.OIDCIssuer = str("GOTCHA_OIDC_ISSUER", "")
	cfg.OIDCClientID = str("GOTCHA_OIDC_CLIENT_ID", "")
	cfg.OIDCClientSecret = str("GOTCHA_OIDC_CLIENT_SECRET", "")
	cfg.OIDCScopes = str("GOTCHA_OIDC_SCOPES", "")
	cfg.OIDCName = str("GOTCHA_OIDC_NAME", "")
	cfg.YandexEnabled = boolEnv("GOTCHA_YANDEX_ENABLED")
	cfg.YandexClientID = str("GOTCHA_YANDEX_CLIENT_ID", "")
	cfg.YandexClientSecret = str("GOTCHA_YANDEX_CLIENT_SECRET", "")
	cfg.VKEnabled = boolEnv("GOTCHA_VK_ENABLED")
	cfg.VKClientID = str("GOTCHA_VK_CLIENT_ID", "")
	cfg.VKClientSecret = str("GOTCHA_VK_CLIENT_SECRET", "")
	if len(errs) > 0 {
		return Config{}, errs[0]
	}

	if cfg.RetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_RETENTION_DAYS must be >= 1, got %d", cfg.RetentionDays)
	}
	if cfg.SpanRetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_SPAN_RETENTION_DAYS must be >= 1, got %d", cfg.SpanRetentionDays)
	}
	if cfg.MetricRetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_METRIC_RETENTION_DAYS must be >= 1, got %d", cfg.MetricRetentionDays)
	}
	if cfg.MetricEvalInterval < 1 {
		return Config{}, fmt.Errorf("GOTCHA_METRIC_EVAL_INTERVAL must be >= 1, got %d", cfg.MetricEvalInterval)
	}
	if cfg.ProfileRetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_PROFILE_RETENTION_DAYS must be >= 1, got %d", cfg.ProfileRetentionDays)
	}
	if cfg.OutboxRetentionDays < 1 {
		return Config{}, fmt.Errorf("GOTCHA_OUTBOX_RETENTION_DAYS must be >= 1, got %d", cfg.OutboxRetentionDays)
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

	if cfg.OIDCEnabled && (cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "") {
		return Config{}, fmt.Errorf("GOTCHA_OIDC_ENABLED requires GOTCHA_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET")
	}
	if cfg.YandexEnabled && (cfg.YandexClientID == "" || cfg.YandexClientSecret == "") {
		return Config{}, fmt.Errorf("GOTCHA_YANDEX_ENABLED requires GOTCHA_YANDEX_CLIENT_ID and _CLIENT_SECRET")
	}
	if cfg.VKEnabled && (cfg.VKClientID == "" || cfg.VKClientSecret == "") {
		return Config{}, fmt.Errorf("GOTCHA_VK_ENABLED requires GOTCHA_VK_CLIENT_ID and _CLIENT_SECRET")
	}

	return cfg, nil
}
