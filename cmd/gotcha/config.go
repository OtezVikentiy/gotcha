package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/baseurl"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

// переводит имена полей структуры (MaxRows, MaxBytes...) в имена переменных окружения:
// оператору имя поля Go ни о чём не говорит, а имя переменной — то, что он правит в .env
var exportFieldEnvNames = map[string]string{
	"MaxRows":    "GOTCHA_EXPORT_MAX_ROWS",
	"MaxBytes":   "GOTCHA_EXPORT_MAX_BYTES",
	"DiskBudget": "GOTCHA_EXPORT_DISK_BUDGET_BYTES",
	"TTL":        "GOTCHA_EXPORT_RETENTION_HOURS",
}

// матч только целым словом (\b): без этого подстрока "TTL" ловит и leaseTTL
// (несвязанная внутренняя константа worker.go), уродуя чужое сообщение
var exportFieldNameRe = regexp.MustCompile(`\b(` + strings.Join(exportFieldNames(), "|") + `)\b`)

// сортировка держит порядок альтернатив в регэкспе детерминированным между прогонами
func exportFieldNames() []string {
	names := make([]string, 0, len(exportFieldEnvNames))
	for name := range exportFieldEnvNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func translateExportEnvNames(msg string) string {
	return exportFieldNameRe.ReplaceAllStringFunc(msg, func(field string) string {
		return exportFieldEnvNames[field]
	})
}

const devSecretKey = "insecure-dev-secret"

type Config struct {
	Mode          string // ingest | web | uptime | probe | all
	Addr          string
	BaseURL       string
	PostgresDSN   string
	ClickHouseDSN string
	SMTPHost      string
	SMTPPort      int
	SMTPUser      string
	SMTPPassword  string
	SMTPFrom      string
	// пусто — https://api.telegram.org, дефолт живёт в пакете notify
	TelegramAPIBase string
	// 0 = хранить вечно (TTL в ClickHouse снимается); исключение — OutboxRetentionDays,
	// у неё пол >= 1: outbox — рабочая очередь, не архив
	RetentionDays        int
	SpanRetentionDays    int
	MetricRetentionDays  int
	ProfileRetentionDays int
	LogRetentionDays     int
	// свой, а не общий с событиями: у инцидента нет телеметрии в ClickHouse,
	// а публичная статус-страница обещает историю за девяносто дней
	IncidentRetentionDays int
	// свой, а не общий с событиями: история выкладок не привязана к телеметрии
	// в ClickHouse, а таблицу пишет публичный ключ приёма вне квоты
	DeployRetentionDays     int
	Edition                 string
	DefaultEventQuota       int64
	DefaultTransactionQuota int64
	DefaultMetricQuota      int64
	DefaultProfileQuota     int64
	DefaultLogQuota         int64
	MaxEventBytes           int64
	// незаданная (0) — дефолт пакета писателя (256 МиБ) либо авто-вывод от потолка кучи;
	// явный 0 или отрицательное — ошибка конфигурации, а не тихий откат к дефолту.
	// SpanWriter — два буфера под одним потолком: худший случай шесть единиц, а не пять.
	MaxBufferBytes int64
	// per-DSN токен-бакет, запросов/с на project id; burst = 2×лимит, 0 выключает;
	// срабатывает после аутентификации ключа и до квоты, ответ 429
	IngestRateLimit int
	// незаданная (0) — дефолт пакета (64 МиБ); явный 0 или отрицательное — ошибка конфигурации
	MaxQueueBytes int64
	// пер-проектный потолок уведомлений; 0 у лимита выключает ограничение
	AlertBudgetWindowSeconds int
	AlertBudgetLimit         int
	// потолок различных значений открытых полей на проект за окно (имя транзакции,
	// окружение, метрика, сервис, операция); 0 выключает ограничение
	CardinalityLimit         int
	CardinalityWindowSeconds int
	RunEvaluators            *bool
	MetricEvalInterval       int
	ProfileEvalInterval      int
	HostEvalInterval         int
	SLOEvalInterval          int
	EscalationInterval       int
	// должна быть >= самого медленного порога тишины хостов-родителей проекта,
	// иначе окно не накрывает реальную гонку детекции
	DependencySettleSeconds int
	OutboxRetentionDays     int
	// 0 выключает сверку удалённых проектов, не отключая саму очередь удаления
	PurgeReconcileHours int
	NotifyConcurrency   int
	SecretKey           string
	SecretKeyPrev       string
	AllowInsecureSecret bool
	// пусто — X-Forwarded-For не доверяется, лимитер ключуется по RemoteAddr
	TrustedProxies   []*net.IPNet
	RegistrationMode string
	// max-age=0 законное значение (снимает пин браузера), а не «выключено»;
	// includeSubDomains по умолчанию false — инстанс часто живёт на поддомене
	HSTSEnabled           bool
	HSTSMaxAgeSeconds     int
	HSTSIncludeSubDomains bool
	HSTSPreload           bool
	// для внешних уведомлений (email/Telegram/webhook) — у получателя нет своей
	// локали вне HTTP-запроса; UI использует локаль зрителя, не эту
	Locale      string
	MigrateOnly bool
	// -1 — не запрошено; force не доделывает миграцию, а снимает признак незавершённости
	MigrateForcePG int
	MigrateForceCH int

	ScrubIP    bool
	ScrubEmail bool
	ScrubKeys  []string
	// матч denylist подстрочный и fail-closed, поэтому author/tokenizer маскируются
	// по умолчанию; исключения оператор задаёт явно через GOTCHA_SCRUB_KEEP_KEYS
	ScrubAllowKeys []string

	LogLevel  string
	LogFormat string
	// по умолчанию выключено; маскирует только email, не номера
	ScrubFreeText bool

	// по умолчанию всё false; риск у контуров разный — Webhook отдаёт ответ цели (до 1 КБ)
	// в UI, OIDC уносит client_secret на token_endpoint, Uptime и Telegram безопаснее
	SSRFAllowPrivateUptime   bool
	SSRFAllowPrivateWebhook  bool
	SSRFAllowPrivateOIDC     bool
	SSRFAllowPrivateTelegram bool
	AutoMigrate              bool
	// privacy-by-default: недоверенным получателям уходит только обезличенная ссылка
	ExternalChannelDetails bool
	// совпадение суффиксом: «corp.example» покрывает и «mail.corp.example»;
	// домен из BaseURL доверен всегда и без этой настройки (alert.NewDetailPolicy)
	TrustedRecipients []string

	UptimeConcurrency int
	LocalRegion       string
	// обязательны только в --mode=probe: пробе больше знать нечего, PG/CH она не открывает
	ProbeToken string
	ServerURL  string

	// дефолт совпадает с ENV GOTCHA_DIST_DIR в Dockerfile; в dev-режиме (go run без
	// docker) каталога физически нет — agentDistAvailable() отдаёт 404 с подсказкой
	AgentDistDir string
	// ключ лимитера — клиентский IP, парк за одним egress-адресом (NAT) делит бюджет;
	// 120/мин — 60 хостов в минуту с одного IP, с запасом для обычной раскатки
	AgentDistRatePerMin int

	// каталог переживает пересоздание контейнера (именованный том exportdata);
	// если создать не удаётся — раздел выключается, продукт работает дальше без него
	ExportDir      string
	ExportTTLHours int
	// обязан быть строго меньше export.eventStreamSafetyLimit (1 000 000) —
	// это проверяет export.Config.Validate() при построении воркера, не здесь
	ExportMaxRows  int64
	ExportMaxBytes int64
	// переполнение — временный отказ новой заявки (до трёх попыток), а не
	// частично записанный файл
	ExportDiskBudgetBytes int64

	OIDCEnabled        bool
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCScopes         string
	OIDCName           string
	OIDCTrustEmail     bool
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

// список живёт в internal/ingest: та же маска применяется в internal/export к
// выгрузкам, и два независимых списка разъехались бы при первой же правке одного
func defaultScrubKeys() []string {
	return ingest.DefaultDenyKeys()
}

func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ingest и uptime тоже расшифровывают секреты каналов доставки, не только web/all;
// probe исключён — он не ходит ни в PG, ни в CH
func secretKeyMattersFor(mode string) bool {
	switch mode {
	case "web", "all", "ingest", "uptime":
		return true
	default:
		return false
	}
}

// уже, чем secretKeyMattersFor: ingest/uptime используют ключ, но web.Handler не
// поднимают — предупреждение про не-https BaseURL там не про что показывать
func hstsHeaderMattersFor(mode string) bool {
	switch mode {
	case "web", "all":
		return true
	default:
		return false
	}
}

// на ошибке разбора возвращает def, а не частичный результат strconv.ParseInt:
// на «8MiB» тот дал бы 0, на переполнении — значение, зажатое до края int64
func parseInt64Env(getenv func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// bitSize = strconv.IntSize, поэтому int(n) без усечения на любой платформе
func parseIntEnv(getenv func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, strconv.IntSize)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return int(n), nil
}

// validateLogging и setupLogging (main.go) читают эту же таблицу — вторую копию,
// которая могла бы разойтись при добавлении уровня, заводить незачем
var logLevels = map[string]slog.Level{
	"":        slog.LevelInfo,
	"info":    slog.LevelInfo,
	"debug":   slog.LevelDebug,
	"warn":    slog.LevelWarn,
	"warning": slog.LevelWarn,
	"error":   slog.LevelError,
}

var logFormats = map[string]bool{
	"":     true,
	"text": true,
	"json": true,
}

// level/format ожидаются уже триммленными и в нижнем регистре, как cfg.LogLevel/LogFormat
func validateLogging(level, format string) error {
	var errs []error
	if _, ok := logLevels[level]; !ok {
		errs = append(errs, fmt.Errorf("GOTCHA_LOGGING_LEVEL must be debug, info, warn (alias warning) or error, got %q", level))
	}
	if _, ok := logFormats[format]; !ok {
		errs = append(errs, fmt.Errorf("GOTCHA_LOGGING_FORMAT must be text or json, got %q", format))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func loadConfig(getenv func(string) string, args []string) (Config, error) {
	// проверяется до security-проверки GOTCHA_SECRET_KEY ниже: старое имя значит,
	// что часть .env не прочиталась вообще, и это важнее знать первым
	if err := envcontract.CheckRenamedAll(getenv); err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("gotcha", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process role: ingest | web | uptime | probe | all")
	migrateOnly := fs.Bool("migrate-only", false,
		"apply schema migrations and exit (init-job for deployments with GOTCHA_AUTO_MIGRATE_ENABLED=false)")
	migrateForce := fs.Int("migrate-force", -1,
		"clear the dirty flag on the PostgreSQL schema at version N and exit (see upgrade docs)")
	migrateForceCH := fs.Int("migrate-force-ch", -1,
		"clear the dirty flag on the ClickHouse schema at version N and exit (see upgrade docs)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if !validModes[*mode] {
		return Config{}, fmt.Errorf("invalid --mode %q: want ingest, web, uptime, probe or all", *mode)
	}
	// probe не открывает соединения с базой: молча выйти нулём было бы обманом
	if *migrateOnly && *mode == "probe" {
		return Config{}, fmt.Errorf("--migrate-only is not valid with --mode=probe: a probe never opens the database")
	}
	if (*migrateForce >= 0 || *migrateForceCH >= 0) && *mode == "probe" {
		return Config{}, fmt.Errorf("--migrate-force is not valid with --mode=probe: a probe never opens the database")
	}
	if (*migrateForce >= 0 || *migrateForceCH >= 0) && *migrateOnly {
		return Config{}, fmt.Errorf("--migrate-force and --migrate-only are different intents (clear the dirty flag vs apply migrations): pass one")
	}
	if *migrateForce >= 0 && *migrateForceCH >= 0 {
		return Config{}, fmt.Errorf("--migrate-force and --migrate-force-ch cannot be combined: migrations run sequentially, only one database can be stuck dirty")
	}

	// строка из одних пробелов — то же самое, что переменная не задана вовсе
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	var errs []error

	// как str, но для переменных, где тихий откат на def при непустом-но-пробельном
	// значении опаснее отказа старта (GOTCHA_SECRET_KEY, GOTCHA_PG_DSN/GOTCHA_CH_DSN)
	strGuarded := func(key, def string) string {
		raw := getenv(key)
		if raw == "" {
			return def
		}
		if v := strings.TrimSpace(raw); v != "" {
			return v
		}
		errs = append(errs, fmt.Errorf(
			"%s must not be blank (got only whitespace); unset the variable entirely to use the default",
			key))
		return def
	}

	// возвращает (значение, задано-ли-непустое); непустое нераспознанное копит
	// ошибку, а не молча выключает дефолт (напр. `SCRUB_IP=ture`)
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

	boolEnvDef := func(key string, def bool) bool {
		v, set := parseBool(key)
		if !set {
			return def
		}
		return v
	}

	num := func(key string, def int64) int64 {
		n, err := parseInt64Env(getenv, key, def)
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}

	intNum := func(key string, def int) int {
		n, err := parseIntEnv(getenv, key, def)
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}

	// редакция определяет дефолт квот: oss безлимит (0), saas 1_000_000;
	// явный GOTCHA_DEFAULT_*_QUOTA перекрывает
	edition := strings.ToLower(str("GOTCHA_EDITION", "oss"))
	defQuota := int64(0)
	if edition == "saas" {
		defQuota = 1_000_000
	}

	// тристабильный: nil, если GOTCHA_EVALUATORS_ENABLED не задан
	var runEvaluators *bool
	if v, set := parseBool("GOTCHA_EVALUATORS_ENABLED"); set {
		runEvaluators = &v
	}

	// num() отдаёт тот же def=0 и при отсутствии переменной, и после ошибки разбора —
	// «задано ли явно» берём до num(), иначе мусор и явный 0 задвоят сообщение
	maxBufferBytesSet := strings.TrimSpace(getenv("GOTCHA_MAX_WRITER_BUFFER_BYTES")) != ""
	maxBufferBytesErrsBefore := len(errs)
	maxBufferBytes := num("GOTCHA_MAX_WRITER_BUFFER_BYTES", 0)
	if maxBufferBytesSet && len(errs) == maxBufferBytesErrsBefore && maxBufferBytes < 1 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_MAX_WRITER_BUFFER_BYTES must be >= 1 (0 or negative refuses startup here, unlike "+
				"the other retention/quota variables where 0 means unlimited/forever; unset the "+
				"variable entirely to use the writer package default), got %d", maxBufferBytes))
	}

	maxQueueBytesSet := strings.TrimSpace(getenv("GOTCHA_MAX_INGEST_QUEUE_BYTES")) != ""
	maxQueueBytesErrsBefore := len(errs)
	maxQueueBytes := num("GOTCHA_MAX_INGEST_QUEUE_BYTES", 0)
	if maxQueueBytesSet && len(errs) == maxQueueBytesErrsBefore && maxQueueBytes < 1 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_MAX_INGEST_QUEUE_BYTES must be >= 1 (0 or negative refuses startup here, unlike "+
				"the other retention/quota variables where 0 means unlimited/forever; unset the "+
				"variable entirely to use the ingest queue package default), got %d", maxQueueBytes))
	}

	cfg := Config{
		Mode:                     *mode,
		MigrateOnly:              *migrateOnly,
		MigrateForcePG:           *migrateForce,
		MigrateForceCH:           *migrateForceCH,
		Addr:                     str("GOTCHA_LISTEN_ADDR", ":8080"),
		BaseURL:                  str("GOTCHA_BASE_URL", "http://localhost:8080"),
		PostgresDSN:              strGuarded("GOTCHA_PG_DSN", "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable"),
		ClickHouseDSN:            strGuarded("GOTCHA_CH_DSN", "clickhouse://localhost:9000/gotcha"),
		SMTPHost:                 str("GOTCHA_SMTP_HOST", ""),
		SMTPPort:                 intNum("GOTCHA_SMTP_PORT", 587),
		SMTPUser:                 str("GOTCHA_SMTP_USER", ""),
		SMTPPassword:             str("GOTCHA_SMTP_PASSWORD", ""),
		SMTPFrom:                 str("GOTCHA_SMTP_FROM", ""),
		TelegramAPIBase:          str("GOTCHA_TELEGRAM_API_BASE", ""),
		RetentionDays:            intNum("GOTCHA_EVENT_RETENTION_DAYS", 90),
		SpanRetentionDays:        intNum("GOTCHA_SPAN_RETENTION_DAYS", 30),
		MetricRetentionDays:      intNum("GOTCHA_METRIC_RETENTION_DAYS", 30),
		ProfileRetentionDays:     intNum("GOTCHA_PROFILE_RETENTION_DAYS", 7),
		LogRetentionDays:         intNum("GOTCHA_LOG_RETENTION_DAYS", 14),
		IncidentRetentionDays:    intNum("GOTCHA_INCIDENT_RETENTION_DAYS", 90),
		DeployRetentionDays:      intNum("GOTCHA_DEPLOY_RETENTION_DAYS", 90),
		Edition:                  edition,
		DefaultEventQuota:        num("GOTCHA_DEFAULT_EVENT_QUOTA", defQuota),
		DefaultTransactionQuota:  num("GOTCHA_DEFAULT_TRANSACTION_QUOTA", defQuota),
		DefaultMetricQuota:       num("GOTCHA_DEFAULT_METRIC_QUOTA", defQuota),
		DefaultProfileQuota:      num("GOTCHA_DEFAULT_PROFILE_QUOTA", defQuota),
		DefaultLogQuota:          num("GOTCHA_DEFAULT_LOG_QUOTA", defQuota),
		MaxEventBytes:            num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		IngestRateLimit:          intNum("GOTCHA_INGEST_RATE_PER_SEC", 500),
		MaxBufferBytes:           maxBufferBytes,
		MaxQueueBytes:            maxQueueBytes,
		AlertBudgetWindowSeconds: intNum("GOTCHA_ALERT_BUDGET_WINDOW_SECONDS", 3600),
		AlertBudgetLimit:         intNum("GOTCHA_ALERT_BUDGET_LIMIT", 50),
		CardinalityLimit:         intNum("GOTCHA_CARDINALITY_LIMIT", 10000),
		CardinalityWindowSeconds: intNum("GOTCHA_CARDINALITY_WINDOW_SECONDS", 3600),
		RunEvaluators:            runEvaluators,
		MetricEvalInterval:       intNum("GOTCHA_METRIC_EVAL_INTERVAL_SECONDS", 60),
		ProfileEvalInterval:      intNum("GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS", 300),
		HostEvalInterval:         intNum("GOTCHA_HOST_EVAL_INTERVAL_SECONDS", 60),
		SLOEvalInterval:          intNum("GOTCHA_SLO_EVAL_INTERVAL_SECONDS", 120),
		EscalationInterval:       intNum("GOTCHA_ESCALATION_INTERVAL_SECONDS", 60),
		DependencySettleSeconds:  intNum("GOTCHA_DEPENDENCY_SETTLE_SECONDS", 300),
		OutboxRetentionDays:      intNum("GOTCHA_OUTBOX_RETENTION_DAYS", 7),
		PurgeReconcileHours:      intNum("GOTCHA_PROJECT_PURGE_RECONCILE_HOURS", 24),
		NotifyConcurrency:        intNum("GOTCHA_NOTIFY_CONCURRENCY", 4),
		SecretKey:                strGuarded("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
		// читается дословно, не через str()/strGuarded(): должен держать старый
		// ключ байт в байт, иначе расшифровка пойдёт уже другим ключом
		SecretKeyPrev:         getenv("GOTCHA_SECRET_KEY_PREV"),
		RegistrationMode:      strings.ToLower(str("GOTCHA_REGISTRATION_MODE", "invite")),
		HSTSEnabled:           boolEnvDef("GOTCHA_HSTS_ENABLED", true),
		HSTSMaxAgeSeconds:     intNum("GOTCHA_HSTS_MAX_AGE_SECONDS", 31536000),
		HSTSIncludeSubDomains: boolEnv("GOTCHA_HSTS_INCLUDE_SUBDOMAINS"),
		HSTSPreload:           boolEnv("GOTCHA_HSTS_PRELOAD"),
		Locale:                strings.ToLower(str("GOTCHA_LOCALE", "ru")),
		UptimeConcurrency:     intNum("GOTCHA_UPTIME_CONCURRENCY", 50),
		LocalRegion:           str("GOTCHA_UPTIME_LOCAL_REGION", "local"),
		ProbeToken:            str("GOTCHA_PROBE_KEY", ""),
		ServerURL:             str("GOTCHA_PROBE_SERVER_URL", ""),
		AgentDistDir:          str("GOTCHA_DIST_DIR", "/opt/gotcha/agent-dist"),
		AgentDistRatePerMin:   intNum("GOTCHA_DIST_RATE_PER_MIN", 120),
		ExportDir:             str("GOTCHA_EXPORT_DIR", "/var/lib/gotcha/exports"),
		ExportTTLHours:        intNum("GOTCHA_EXPORT_RETENTION_HOURS", 168),
		ExportMaxRows:         num("GOTCHA_EXPORT_MAX_ROWS", 200_000),
		ExportMaxBytes:        num("GOTCHA_EXPORT_MAX_BYTES", 268_435_456),
		ExportDiskBudgetBytes: num("GOTCHA_EXPORT_DISK_BUDGET_BYTES", 5_368_709_120),
	}
	// читается дословно, минуя blank-проверку strGuarded(); тот же контракт
	// повторён здесь явно, безусловно, вне secretKeyMattersFor(cfg.Mode)
	if raw := cfg.SecretKeyPrev; raw != "" && strings.TrimSpace(raw) == "" {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_SECRET_KEY_PREV must not be blank (got only whitespace); "+
				"unset the variable entirely if no rotation is in progress"))
	}
	// парсером самого клиента (pgxpool.ParseConfig/clickhouse.ParseDSN), не baseurl.Normalize:
	// DSN допускает и keyword/value-форму (host=… user=…), не только URL
	if err := db.ValidatePostgresDSN(cfg.PostgresDSN); err != nil {
		errs = append(errs, fmt.Errorf("GOTCHA_PG_DSN: %w", err))
	}
	if err := db.ValidateClickHouseDSN(cfg.ClickHouseDSN); err != nil {
		errs = append(errs, fmt.Errorf("GOTCHA_CH_DSN: %w", err))
	}
	// один хелпер на все базовые адреса: без схемы/хоста ссылка ведёт в никуда,
	// хвостовая «/» даёт «//dashboard» молча
	normalizedBaseURL, baseURLErr := baseurl.Normalize("GOTCHA_BASE_URL", cfg.BaseURL)
	if baseURLErr != nil {
		errs = append(errs, baseURLErr)
	} else {
		cfg.BaseURL = normalizedBaseURL
	}
	cfg.OIDCEnabled = boolEnv("GOTCHA_OIDC_ENABLED")
	cfg.OIDCIssuer = str("GOTCHA_OIDC_ISSUER", "")
	cfg.OIDCClientID = str("GOTCHA_OIDC_CLIENT_ID", "")
	cfg.OIDCClientSecret = str("GOTCHA_OIDC_CLIENT_SECRET", "")
	cfg.OIDCScopes = str("GOTCHA_OIDC_SCOPES", "")
	cfg.OIDCName = str("GOTCHA_OIDC_DISPLAY_NAME", "")
	cfg.OIDCTrustEmail = boolEnv("GOTCHA_OIDC_TRUST_EMAIL")
	cfg.YandexEnabled = boolEnv("GOTCHA_YANDEX_ENABLED")
	cfg.YandexClientID = str("GOTCHA_YANDEX_CLIENT_ID", "")
	cfg.YandexClientSecret = str("GOTCHA_YANDEX_CLIENT_SECRET", "")
	cfg.VKEnabled = boolEnv("GOTCHA_VK_ENABLED")
	cfg.VKClientID = str("GOTCHA_VK_CLIENT_ID", "")
	cfg.VKClientSecret = str("GOTCHA_VK_CLIENT_SECRET", "")

	cfg.ScrubIP = boolEnvDef("GOTCHA_SCRUB_IP", true)
	cfg.ScrubEmail = boolEnvDef("GOTCHA_SCRUB_EMAIL", true)
	cfg.ScrubFreeText = boolEnv("GOTCHA_SCRUB_FREETEXT")
	// общий флаг — дефолт для трёх раздельных, для обратной совместимости
	ssrfAll := boolEnv("GOTCHA_SSRF_ALLOW_PRIVATE")
	cfg.SSRFAllowPrivateUptime = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME", ssrfAll)
	cfg.SSRFAllowPrivateWebhook = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK", ssrfAll)
	cfg.SSRFAllowPrivateOIDC = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_OIDC", ssrfAll)
	cfg.SSRFAllowPrivateTelegram = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_TELEGRAM", ssrfAll)
	cfg.AutoMigrate = boolEnvDef("GOTCHA_AUTO_MIGRATE_ENABLED", true)
	// --migrate-only подразумевает применение миграций даже при GOTCHA_AUTO_MIGRATE_ENABLED=false
	if cfg.MigrateOnly {
		cfg.AutoMigrate = true
	}
	cfg.ExternalChannelDetails = boolEnvDef("GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED", false)
	// дополняет дефолтный denylist, а не заменяет его — убрать конкретный дефолт
	// можно только точным именем через GOTCHA_SCRUB_KEEP_KEYS
	cfg.ScrubKeys = defaultScrubKeys()
	for _, k := range strings.Split(getenv("GOTCHA_SCRUB_DENY_KEYS"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			cfg.ScrubKeys = append(cfg.ScrubKeys, k)
		}
	}
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_LEVEL")))
	cfg.LogFormat = strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_FORMAT")))

	for _, k := range strings.Split(getenv("GOTCHA_SCRUB_KEEP_KEYS"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			cfg.ScrubAllowKeys = append(cfg.ScrubAllowKeys, k)
		}
	}
	// невалидное имя тут не опасно (просто ни с чем не совпадёт), в отличие от TRUSTED_PROXIES
	for _, h := range strings.Split(getenv("GOTCHA_TRUSTED_RECIPIENTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			cfg.TrustedRecipients = append(cfg.TrustedRecipients, h)
		}
	}
	// невалидные записи — ошибка конфигурации: молча проигнорированный прокси
	// означал бы, что лимитер снова ключуется по IP прокси, а не клиента
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
	// разбирается безусловно: на сильном ключе короткое замыкание проверок ниже не
	// дошло бы до boolEnv, и мусор («ture» и т.п.) не попал бы в errs
	cfg.AllowInsecureSecret = boolEnv("GOTCHA_SECRET_KEY_ALLOW_INSECURE")

	// дефолтный ключ публично известен из исходников; на не-localhost BaseURL это угон
	// аккаунта через OAuth-link. baseURLErr == nil: невалидный BaseURL — не «публичный»
	if secretKeyMattersFor(cfg.Mode) &&
		cfg.SecretKey == devSecretKey &&
		baseURLErr == nil &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!cfg.AllowInsecureSecret {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY must be set to a strong random value for a non-local %s instance "+
				"(default key is public and enables OAuth account takeover); "+
				"set GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 to override for development", cfg.Mode)
	}

	// в серверных режимах на не-local требуем >= 32 байт, тот же escape-hatch
	// и та же оговорка про baseURLErr, что у проверки дефолтного ключа выше
	if secretKeyMattersFor(cfg.Mode) &&
		cfg.SecretKey != devSecretKey &&
		len(cfg.SecretKey) < 32 &&
		baseURLErr == nil &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!cfg.AllowInsecureSecret {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY is too short (%d bytes) for a non-local %s instance; "+
				"use at least 32 random bytes (e.g. `openssl rand -hex 32`); "+
				"set GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 to override for development",
			len(cfg.SecretKey), cfg.Mode)
	}

	if secretKeyMattersFor(cfg.Mode) && cfg.SecretKeyPrev != "" {
		switch {
		case cfg.SecretKeyPrev == devSecretKey:
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV must not be the public dev default " +
					"(nothing was ever encrypted with it, so it cannot be a rotation source); " +
					"unset GOTCHA_SECRET_KEY_PREV if no rotation is in progress")
		case cfg.SecretKeyPrev == cfg.SecretKey:
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV must differ from GOTCHA_SECRET_KEY " +
					"(a rotation source equal to the current key means no rotation " +
					"is actually happening); unset GOTCHA_SECRET_KEY_PREV if none is")
		case cfg.SecretKey == devSecretKey:
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV is set but GOTCHA_SECRET_KEY is still the dev " +
					"default (encryption at rest is off entirely on the dev key, so " +
					"GOTCHA_SECRET_KEY_PREV would silently do nothing); " +
					"set a real GOTCHA_SECRET_KEY or unset GOTCHA_SECRET_KEY_PREV")
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	if err := validateLogging(cfg.LogLevel, cfg.LogFormat); err != nil {
		errs = append(errs, err)
	}

	// проверяется независимо от HSTSEnabled: отрицательное значение — всегда опечатка
	if cfg.HSTSMaxAgeSeconds < 0 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_HSTS_MAX_AGE_SECONDS must be >= 0 (0 sends max-age=0, which un-pins browsers), got %d",
			cfg.HSTSMaxAgeSeconds))
	}
	// только при включённом HSTS: иначе аварийный откат «выключить HSTS» упёрся
	// бы в отказ старта ровно тогда, когда сервис и так лежит
	if cfg.HSTSEnabled && cfg.HSTSPreload {
		if !cfg.HSTSIncludeSubDomains {
			errs = append(errs, fmt.Errorf(
				"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_INCLUDE_SUBDOMAINS=true: "+
					"the preload list rejects a header without includeSubDomains"))
		}
		if cfg.HSTSMaxAgeSeconds < 31536000 {
			errs = append(errs, fmt.Errorf(
				"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_MAX_AGE_SECONDS >= 31536000 (one year), got %d",
				cfg.HSTSMaxAgeSeconds))
		}
	}
	if cfg.HSTSEnabled {
		if hstsHeaderMattersFor(cfg.Mode) && !strings.HasPrefix(cfg.BaseURL, "https://") {
			slog.Warn("GOTCHA_HSTS_ENABLED is on but GOTCHA_BASE_URL is not https:// — " +
				"Strict-Transport-Security is never sent on a plain HTTP deploy")
		}
	} else {
		// факт присутствия переменной, не её значение: boolEnvDef/boolEnv не
		// отличают «выставлено в дефолт» от «не выставлено»
		for _, name := range []string{
			"GOTCHA_HSTS_MAX_AGE_SECONDS",
			"GOTCHA_HSTS_INCLUDE_SUBDOMAINS",
			"GOTCHA_HSTS_PRELOAD",
		} {
			if strings.TrimSpace(getenv(name)) != "" {
				slog.Warn(fmt.Sprintf(
					"HSTS is off (GOTCHA_HSTS_ENABLED=false) — %s is ignored", name),
					"var", name)
			}
		}
	}

	switch cfg.RegistrationMode {
	case "open", "invite", "closed":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_REGISTRATION_MODE must be open, invite or closed, got %q", cfg.RegistrationMode))
	}

	switch cfg.Locale {
	case "ru", "en":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_LOCALE must be ru or en, got %q", cfg.Locale))
	}

	switch cfg.Edition {
	case "oss", "saas":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_EDITION must be oss or saas, got %q", cfg.Edition))
	}

	if cfg.RetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_EVENT_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.RetentionDays))
	}
	if cfg.SpanRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_SPAN_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.SpanRetentionDays))
	}
	if cfg.MetricRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_METRIC_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.MetricRetentionDays))
	}
	if cfg.AlertBudgetWindowSeconds < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_ALERT_BUDGET_WINDOW_SECONDS must be >= 1, got %d", cfg.AlertBudgetWindowSeconds))
	}
	if cfg.AlertBudgetLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_ALERT_BUDGET_LIMIT must be >= 0 (0 disables the ceiling), got %d", cfg.AlertBudgetLimit))
	}
	if cfg.IngestRateLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_INGEST_RATE_PER_SEC must be >= 0 (0 disables the limit), got %d", cfg.IngestRateLimit))
	}
	if cfg.CardinalityLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_CARDINALITY_LIMIT must be >= 0 (0 disables the limit), got %d", cfg.CardinalityLimit))
	}
	if cfg.CardinalityWindowSeconds < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_CARDINALITY_WINDOW_SECONDS must be >= 1, got %d", cfg.CardinalityWindowSeconds))
	}
	if cfg.MetricEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_METRIC_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.MetricEvalInterval))
	}
	if cfg.ProfileRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROFILE_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.ProfileRetentionDays))
	}
	if cfg.LogRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_LOG_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.LogRetentionDays))
	}
	if cfg.IncidentRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_INCIDENT_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.IncidentRetentionDays))
	}
	if cfg.DeployRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEPLOY_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.DeployRetentionDays))
	}
	if cfg.OutboxRetentionDays < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_OUTBOX_RETENTION_DAYS must be >= 1, got %d", cfg.OutboxRetentionDays))
	}
	if cfg.PurgeReconcileHours < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROJECT_PURGE_RECONCILE_HOURS must be >= 0, got %d", cfg.PurgeReconcileHours))
	}
	if cfg.NotifyConcurrency < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_NOTIFY_CONCURRENCY must be >= 1, got %d", cfg.NotifyConcurrency))
	}
	if cfg.ProfileEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.ProfileEvalInterval))
	}
	if cfg.HostEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_HOST_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.HostEvalInterval))
	}
	if cfg.SLOEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_SLO_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.SLOEvalInterval))
	}
	if cfg.EscalationInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_ESCALATION_INTERVAL_SECONDS must be >= 1, got %d", cfg.EscalationInterval))
	}
	if cfg.DependencySettleSeconds < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEPENDENCY_SETTLE_SECONDS must be >= 0, got %d", cfg.DependencySettleSeconds))
	}
	if cfg.DefaultEventQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_EVENT_QUOTA must be >= 0, got %d", cfg.DefaultEventQuota))
	}
	if cfg.DefaultTransactionQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_TRANSACTION_QUOTA must be >= 0, got %d", cfg.DefaultTransactionQuota))
	}
	if cfg.DefaultMetricQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_METRIC_QUOTA must be >= 0, got %d", cfg.DefaultMetricQuota))
	}
	if cfg.DefaultProfileQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_PROFILE_QUOTA must be >= 0, got %d", cfg.DefaultProfileQuota))
	}
	if cfg.DefaultLogQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_LOG_QUOTA must be >= 0, got %d", cfg.DefaultLogQuota))
	}
	if cfg.MaxEventBytes < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_MAX_EVENT_BYTES must be >= 1, got %d", cfg.MaxEventBytes))
	}
	if cfg.UptimeConcurrency < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_UPTIME_CONCURRENCY must be >= 1, got %d", cfg.UptimeConcurrency))
	}
	// проверяется независимо от того, задан ли GOTCHA_SMTP_HOST: мусор в порту —
	// опечатка уже сейчас, а не при первой попытке отправки месяцами позже
	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		errs = append(errs, fmt.Errorf("GOTCHA_SMTP_PORT must be between 1 and 65535, got %d", cfg.SMTPPort))
	}
	// дублирует ту же export.Config.Validate(), что worker.Tick вызывает на каждом
	// тике — без старт-проверки процесс выглядел здоровым, а заявки копились навсегда
	if err := (export.Config{
		TTL:        time.Duration(cfg.ExportTTLHours) * time.Hour,
		MaxRows:    cfg.ExportMaxRows,
		MaxBytes:   cfg.ExportMaxBytes,
		DiskBudget: cfg.ExportDiskBudgetBytes,
	}).Validate(); err != nil {
		errs = append(errs, errors.New(translateExportEnvNames(err.Error())))
	}
	// отправитель дописывает «/bot{token}/sendMessage»; хвостовая «/» дала бы
	// «…org//bot…», и Bot API отвечал бы 404 на каждое уведомление
	normalizedTelegramAPIBase, err := baseurl.Normalize("GOTCHA_TELEGRAM_API_BASE", cfg.TelegramAPIBase)
	if err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramAPIBase = normalizedTelegramAPIBase
	}

	// формат-проверку гейтим режимом пробы: вне --mode=probe переменную никто не
	// читает, ронять старт по невалидному-но-неиспользуемому значению незачем
	if cfg.Mode == "probe" {
		normalizedServerURL, err := baseurl.Normalize("GOTCHA_PROBE_SERVER_URL", cfg.ServerURL)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.ServerURL = normalizedServerURL
			if cfg.ServerURL == "" {
				errs = append(errs, fmt.Errorf("GOTCHA_PROBE_SERVER_URL is required with --mode=probe"))
			}
		}
		if cfg.ProbeToken == "" {
			errs = append(errs, fmt.Errorf("GOTCHA_PROBE_KEY is required with --mode=probe"))
		}
	} else if cfg.ServerURL != "" {
		slog.Warn("GOTCHA_PROBE_SERVER_URL is set but --mode is not probe — the value is ignored",
			"mode", cfg.Mode)
	}

	if cfg.OIDCEnabled && (cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_OIDC_ENABLED requires GOTCHA_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET"))
	}
	if cfg.OIDCEnabled && !cfg.OIDCTrustEmail {
		slog.Warn("GOTCHA_OIDC_ENABLED is on but GOTCHA_OIDC_TRUST_EMAIL is not — " +
			"self-registration and account auto-linking by email via this OIDC provider are disabled; " +
			"set GOTCHA_OIDC_TRUST_EMAIL=true only if this IdP is single-tenant and you control who can sign up on it")
	}
	if cfg.YandexEnabled && (cfg.YandexClientID == "" || cfg.YandexClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_YANDEX_ENABLED requires GOTCHA_YANDEX_CLIENT_ID and _CLIENT_SECRET"))
	}
	if cfg.VKEnabled && (cfg.VKClientID == "" || cfg.VKClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_VK_ENABLED requires GOTCHA_VK_CLIENT_ID and _CLIENT_SECRET"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// порядок обязателен: раньше CheckRenamedAll устаревшее имя получит вместо
// «renamed to NEW_NAME» менее точную догадку по Левенштейну (или никакой)
func loadConfigChecked(getenv func(string) string, environ func() []string, args []string) (Config, error) {
	cfg, err := loadConfig(getenv, args)
	if err != nil {
		return Config{}, err
	}
	if err := checkUnknownEnvVars(environ); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// GOTCHA_COMPOSE_*/GOTCHA_BUILD_* исключены целиком по префиксу: их читает Docker
// Compose или Makefile, не Go-процесс; опечатка внутри этих префиксов проходит молча
func checkUnknownEnvVars(environ func() []string) error {
	var renamed, unknown []string
	for _, kv := range environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GOTCHA_") {
			continue
		}
		if envcontract.Known[name] {
			continue
		}
		if strings.HasPrefix(name, "GOTCHA_COMPOSE_") || strings.HasPrefix(name, "GOTCHA_BUILD_") {
			continue
		}
		if _, wasRenamed := envcontract.Renamed[name]; wasRenamed {
			renamed = append(renamed, name)
			continue
		}
		unknown = append(unknown, name)
	}
	var errs []error
	if err := envcontract.RenamedError(renamed); err != nil {
		errs = append(errs, err)
	}
	if len(unknown) == 0 {
		return errors.Join(errs...)
	}
	// иначе порядок в тексте ошибки менялся бы вместе с os.Environ() (не гарантирован)
	sort.Strings(unknown)
	parts := make([]string, len(unknown))
	for i, name := range unknown {
		if suggestions := suggestKnownNames(name); len(suggestions) > 0 {
			parts[i] = fmt.Sprintf("%s (unknown; did you mean %s?)", name, strings.Join(suggestions, " or "))
		} else {
			parts[i] = fmt.Sprintf("%s (unknown)", name)
		}
	}
	errs = append(errs, fmt.Errorf("unknown environment variable(s), check for typos: %s", strings.Join(parts, ", ")))
	return errors.Join(errs...)
}

// 2 ловит опечатки одной буквой или коротким суффиксом, не расползаясь на имена
// из другой подсистемы, случайно похожие длиной и общими буквами
const maxSuggestDistance = 2

// сортировка по (расстояние, алфавит) детерминирует текст ошибки при нескольких
// кандидатах на одном расстоянии — envcontract.Known как map порядок не гарантирует
func suggestKnownNames(name string) []string {
	type candidate struct {
		name string
		dist int
	}
	var candidates []candidate
	for known := range envcontract.Known {
		if d := levenshteinDistance(name, known); d <= maxSuggestDistance {
			candidates = append(candidates, candidate{known, d})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		return candidates[i].name < candidates[j].name
	})
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.name
	}
	return out
}

// две строки-буфера, не полная матрица: O(m) память вместо O(n*m)
func levenshteinDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			deletion := prev[j] + 1
			insertion := curr[j-1] + 1
			substitution := prev[j-1] + cost
			min := deletion
			if insertion < min {
				min = insertion
			}
			if substitution < min {
				min = substitution
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
