package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// devSecretKey — публично известный дефолт GOTCHA_SECRET_KEY для localhost-стендов.
// Вынесен в константу, т.к. по нему принимаются решения в нескольких местах
// (валидация запуска, отказ от бессмысленного at-rest-шифрования SSO — Info21).
const devSecretKey = "insecure-dev-secret"

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
	// Edition — редакция сборки (oss | saas). Влияет на дефолты квот:
	// в oss все дефолты = 0 (безлимит), в saas = 1_000_000. См. loadConfig.
	Edition string
	// Default*Quota — дефолтные месячные квоты приёма при создании
	// организации (0 = безлимит). Читаются из GOTCHA_DEFAULT_*_QUOTA;
	// дефолт зависит от Edition.
	DefaultEventQuota       int64
	DefaultTransactionQuota int64
	DefaultMetricQuota      int64
	DefaultProfileQuota     int64
	MaxEventBytes           int64
	MetricEvalInterval      int
	ProfileEvalInterval     int
	OutboxRetentionDays     int
	SecretKey               string
	// TrustedProxies — CIDR/IP доверенных reverse-proxy (GOTCHA_TRUSTED_PROXIES).
	// Пусто — X-Forwarded-For не доверяется, per-IP лимитер ключуется по
	// RemoteAddr (см. web.clientIP, SEC-L2).
	TrustedProxies []*net.IPNet
	// RegistrationMode — режим самостоятельной регистрации (PROD-B1):
	// open (открыта всем), invite (по приглашению, кроме bootstrap первого
	// админа), closed (только bootstrap первого админа). Дефолт — invite.
	RegistrationMode string

	// ScrubIP/ScrubEmail/ScrubKeys — серверный PII-scrubbing (PRIV-H1),
	// включён по умолчанию. ScrubIP/ScrubEmail зануляют ip/email субъекта;
	// ScrubKeys — denylist ключей, значения которых редактируются в
	// tags/contexts/stacktrace/span.data.
	ScrubIP    bool
	ScrubEmail bool
	ScrubKeys  []string
	// ScrubFreeText (RA-L10) — опционально маскировать email-адреса в свободном
	// тексте (message/exception_value/span.description). По умолчанию выключено
	// (консервативно, чтобы не портить SQL/URL); только email, не номера.
	ScrubFreeText bool

	// SSRFAllowPrivate (SEC-M1) — разрешить uptime-чекерам и webhook'ам
	// ходить на приватные/loopback/link-local адреса. По умолчанию false
	// (мультитенантная защита от SSRF к метадате/внутренним сервисам).
	SSRFAllowPrivate bool
	// AutoMigrate (ARCH-M3) — применять миграции схемы на старте. По
	// умолчанию true; false выносит миграции в отдельный init-job, чтобы
	// app-реплики не клинили все разом на dirty-состоянии.
	AutoMigrate bool
	// ExternalChannelDetails — слать ли текст ошибки (title/culprit/body) во
	// внешние каналы (Telegram/webhook). По умолчанию true; false шлёт только
	// обезличенную ссылку (152-ФЗ: текст может нести ПДн, уезжающие за пределы РФ).
	ExternalChannelDetails bool

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

// defaultScrubKeys — denylist ключей для PII-scrubbing по умолчанию (PRIV-H1).
func defaultScrubKeys() []string {
	return []string{
		"password", "passwd", "token", "secret", "authorization", "auth",
		"cookie", "api_key", "apikey", "access_token", "refresh_token",
		"session", "credit_card", "card_number", "cvv",
	}
}

// isLocalBaseURL — BaseURL указывает на локальную разработку (localhost/loopback).
// Для таких стендов дефолтный SecretKey допустим (см. валидацию ниже).
func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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

	// parseBool — распознаёт булево значение env; для непустого нераспознанного
	// значения копит ошибку (RA-L4: `SCRUB_IP=ture` не должен молча выключать
	// privacy-дефолт). Возвращает (значение, задано-ли-непустое).
	parseBool := func(key string) (bool, bool) {
		v := strings.ToLower(strings.TrimSpace(getenv(key)))
		switch v {
		case "":
			return false, false
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		default:
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q (want 1/0/true/false/yes/no/on/off)", key, getenv(key)))
			return false, true
		}
	}

	boolEnv := func(key string) bool {
		v, _ := parseBool(key)
		return v
	}

	// boolEnvDef — как boolEnv, но unset → def (для флагов, включённых по
	// умолчанию: явные 0/false/no/off → false).
	boolEnvDef := func(key string, def bool) bool {
		v, set := parseBool(key)
		if !set {
			return def
		}
		return v
	}

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

	// PROD-B2: редакция определяет дефолт квот. В oss безлимит (0),
	// в saas — прежний 1_000_000. Явный GOTCHA_DEFAULT_*_QUOTA перекрывает.
	edition := str("GOTCHA_EDITION", "oss")
	defQuota := int64(0)
	if edition == "saas" {
		defQuota = 1_000_000
	}

	cfg := Config{
		Mode:                    *mode,
		Addr:                    str("GOTCHA_ADDR", ":8080"),
		BaseURL:                 str("GOTCHA_BASE_URL", "http://localhost:8080"),
		PostgresDSN:             str("GOTCHA_PG_DSN", "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable"),
		ClickHouseDSN:           str("GOTCHA_CH_DSN", "clickhouse://localhost:9000/gotcha"),
		SMTPHost:                str("GOTCHA_SMTP_HOST", ""),
		SMTPPort:                int(num("GOTCHA_SMTP_PORT", 587)),
		SMTPUser:                str("GOTCHA_SMTP_USER", ""),
		SMTPPassword:            str("GOTCHA_SMTP_PASSWORD", ""),
		SMTPFrom:                str("GOTCHA_SMTP_FROM", ""),
		RetentionDays:           int(num("GOTCHA_RETENTION_DAYS", 90)),
		SpanRetentionDays:       int(num("GOTCHA_SPAN_RETENTION_DAYS", 30)),
		MetricRetentionDays:     int(num("GOTCHA_METRIC_RETENTION_DAYS", 30)),
		ProfileRetentionDays:    int(num("GOTCHA_PROFILE_RETENTION_DAYS", 7)),
		Edition:                 edition,
		DefaultEventQuota:       num("GOTCHA_DEFAULT_EVENT_QUOTA", defQuota),
		DefaultTransactionQuota: num("GOTCHA_DEFAULT_TRANSACTION_QUOTA", defQuota),
		DefaultMetricQuota:      num("GOTCHA_DEFAULT_METRIC_QUOTA", defQuota),
		DefaultProfileQuota:     num("GOTCHA_DEFAULT_PROFILE_QUOTA", defQuota),
		MaxEventBytes:           num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		MetricEvalInterval:      int(num("GOTCHA_METRIC_EVAL_INTERVAL", 60)),
		ProfileEvalInterval:     int(num("GOTCHA_PROFILE_EVAL_INTERVAL", 300)),
		OutboxRetentionDays:     int(num("GOTCHA_OUTBOX_RETENTION_DAYS", 7)),
		SecretKey:               str("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
		RegistrationMode:        str("GOTCHA_REGISTRATION", "invite"),
		UptimeConcurrency:       int(num("GOTCHA_UPTIME_CONCURRENCY", 50)),
		LocalRegion:             str("GOTCHA_LOCAL_REGION", "local"),
		ProbeToken:              str("GOTCHA_PROBE_TOKEN", ""),
		ServerURL:               str("GOTCHA_SERVER_URL", ""),
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

	// PRIV-H1: PII-scrubbing включён по умолчанию.
	cfg.ScrubIP = boolEnvDef("GOTCHA_SCRUB_IP", true)
	cfg.ScrubEmail = boolEnvDef("GOTCHA_SCRUB_EMAIL", true)
	cfg.ScrubFreeText = boolEnv("GOTCHA_SCRUB_FREETEXT")
	cfg.SSRFAllowPrivate = boolEnv("GOTCHA_SSRF_ALLOW_PRIVATE")
	cfg.AutoMigrate = boolEnvDef("GOTCHA_AUTO_MIGRATE", true)
	// Privacy-by-default: полный текст ошибок/стектрейсов/имён транзакций может
	// нести ПДн, а внешние каналы (Telegram — серверы за пределами РФ, webhook)
	// уводят его наружу, потенциально трансгранично (152-ФЗ ст.12). По умолчанию
	// шлём обезличенный payload (только ссылка/заголовок); оператор осознанно
	// включает детали через GOTCHA_EXTERNAL_CHANNEL_DETAILS=true.
	cfg.ExternalChannelDetails = boolEnvDef("GOTCHA_EXTERNAL_CHANNEL_DETAILS", false)
	if keys := strings.TrimSpace(getenv("GOTCHA_SCRUB_KEYS")); keys != "" {
		for _, k := range strings.Split(keys, ",") {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
				cfg.ScrubKeys = append(cfg.ScrubKeys, k)
			}
		}
	} else {
		cfg.ScrubKeys = defaultScrubKeys()
	}
	// GOTCHA_TRUSTED_PROXIES — список CIDR («10.0.0.0/8») и/или голых IP
	// («192.168.1.5», трактуется как /32 или /128) доверенных прокси.
	// Невалидные записи — ошибка конфигурации, а не тихий пропуск: молча
	// проигнорированный прокси означал бы, что XFF не доверяется и лимитер
	// снова ключуется по IP прокси (тихая деградация защиты).
	if tp := strings.TrimSpace(getenv("GOTCHA_TRUSTED_PROXIES")); tp != "" {
		for _, item := range strings.Split(tp, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if !strings.Contains(item, "/") {
				if ip := net.ParseIP(item); ip != nil {
					if ip.To4() != nil {
						item += "/32"
					} else {
						item += "/128"
					}
				}
			}
			_, n, err := net.ParseCIDR(item)
			if err != nil {
				errs = append(errs, fmt.Errorf("GOTCHA_TRUSTED_PROXIES: invalid entry %q: %w", item, err))
				continue
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, n)
		}
	}
	if len(errs) > 0 {
		return Config{}, errs[0]
	}

	switch cfg.RegistrationMode {
	case "open", "invite", "closed":
	default:
		return Config{}, fmt.Errorf("GOTCHA_REGISTRATION must be open, invite or closed, got %q", cfg.RegistrationMode)
	}

	switch cfg.Edition {
	case "oss", "saas":
	default:
		return Config{}, fmt.Errorf("GOTCHA_EDITION must be oss or saas, got %q", cfg.Edition)
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
	if cfg.ProfileEvalInterval < 1 {
		return Config{}, fmt.Errorf("GOTCHA_PROFILE_EVAL_INTERVAL must be >= 1, got %d", cfg.ProfileEvalInterval)
	}
	// Квоты: 0 = безлимит (легитимно в любой редакции), отрицательные — ошибка.
	if cfg.DefaultEventQuota < 0 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_EVENT_QUOTA must be >= 0, got %d", cfg.DefaultEventQuota)
	}
	if cfg.DefaultTransactionQuota < 0 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_TRANSACTION_QUOTA must be >= 0, got %d", cfg.DefaultTransactionQuota)
	}
	if cfg.DefaultMetricQuota < 0 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_METRIC_QUOTA must be >= 0, got %d", cfg.DefaultMetricQuota)
	}
	if cfg.DefaultProfileQuota < 0 {
		return Config{}, fmt.Errorf("GOTCHA_DEFAULT_PROFILE_QUOTA must be >= 0, got %d", cfg.DefaultProfileQuota)
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

	// SEC-C1: дефолтный ключ подписи oauth-cookie публично известен из исходников.
	// В серверных режимах (web/all) на не-localhost BaseURL это дыра (угон аккаунта
	// через OAuth-link) — отказываемся стартовать. Escape-hatch для нестандартного
	// dev-окружения — GOTCHA_ALLOW_INSECURE_SECRET=1.
	if (cfg.Mode == "web" || cfg.Mode == "all") &&
		cfg.SecretKey == devSecretKey &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!boolEnv("GOTCHA_ALLOW_INSECURE_SECRET") {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY must be set to a strong random value for a non-local %s instance "+
				"(default key is public and enables OAuth account takeover); "+
				"set GOTCHA_ALLOW_INSECURE_SECRET=1 to override for development", cfg.Mode)
	}

	// SEC: слишком короткий кастомный ключ — слабый ключ подписи oauth-cookie и
	// мастер шифрования SSO client_secret. В серверных режимах на не-local
	// требуем >= 32 байт (стандартный минимум для ключа). Тот же escape-hatch,
	// что и у проверки дефолтного ключа выше.
	if (cfg.Mode == "web" || cfg.Mode == "all") &&
		cfg.SecretKey != devSecretKey &&
		len(cfg.SecretKey) < 32 &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!boolEnv("GOTCHA_ALLOW_INSECURE_SECRET") {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY is too short (%d bytes) for a non-local %s instance; "+
				"use at least 32 random bytes (e.g. `openssl rand -hex 32`); "+
				"set GOTCHA_ALLOW_INSECURE_SECRET=1 to override for development",
			len(cfg.SecretKey), cfg.Mode)
	}

	return cfg, nil
}
