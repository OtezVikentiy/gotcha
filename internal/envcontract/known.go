package envcontract

// Known — множество ВСЕХ переменных окружения `GOTCHA_*`, которые сегодня
// реально читает хотя бы один из двух бинарей продукта: `cmd/gotcha`
// (единый `config.go` на все режимы — ingest/web/uptime/probe/all, это один
// `Config`, читать его отдельно для режима пробы незачем) и
// `internal/agent` (`gotcha-agent`). Объединение обязательно, а не только
// серверный список: агент штатно ставится на тот же хост, что и сервер,
// с общим `.env` (`install.sh` кладёт агентские переменные рядом с
// серверными) — проверка неизвестных имён, знающая только серверные, отказала
// бы старту сервера на легитимном `GOTCHA_AGENT_INGEST_KEY` соседа по файлу.
//
// Полнота — в обе стороны — доказывается сторожами в
// `internal/guards/env_example_test.go` (тот же AST-сборщик
// `collectGotchaEnvVars`, что уже сверяет `.env.example`, с добавленным
// `parseBool` в `envReaderFuncs`): каждое имя, которое реально читает
// `cmd/gotcha/config.go` или `internal/agent/config.go`, обязано быть здесь
// (иначе новая переменная тихо стала бы «неизвестной» для легитимного
// оператора при следующем релизе), и наоборот — каждое имя здесь обязано
// реально читаться одним из двух (иначе реестр «знает» имена-призраки,
// никем не читаемые, и отказ на неизвестном имени перестаёт быть надёжным).
//
// Единственный рукописный список в дереве: обе стороны сверки живут в
// сторожах, а не во втором ручном перечне — иначе есть риск, что список
// правится в одном месте, а сторож проверяет другое.
//
// Компоузные и build-переменные (`GOTCHA_COMPOSE_*`, `GOTCHA_BUILD_*`) сюда
// не входят и входить не должны: ни один процесс Go их не читает вовсе —
// их читает сам Docker Compose (подстановка `${...}`) либо Makefile
// (build-args образа). Проверка неизвестных имён (`checkUnknownEnvVars`,
// `cmd/gotcha/config.go`) исключает оба префикса отдельным правилом, а не
// перечислением: `env_file: .env` в `docker-compose.yml` легитимно кладёт
// их в окружение процесса `gotcha`, который их не читает и не обязан о них
// знать по имени.
var Known = map[string]bool{
	// cmd/gotcha/config.go — сервер (все режимы: ingest/web/uptime/probe/all)
	"GOTCHA_ALERT_BUDGET_LIMIT":               true,
	"GOTCHA_ALERT_BUDGET_WINDOW_SECONDS":      true,
	"GOTCHA_AUTO_MIGRATE_ENABLED":             true,
	"GOTCHA_BASE_URL":                         true,
	"GOTCHA_CARDINALITY_LIMIT":                true,
	"GOTCHA_CARDINALITY_WINDOW_SECONDS":       true,
	"GOTCHA_CH_DSN":                           true,
	"GOTCHA_DEFAULT_EVENT_QUOTA":              true,
	"GOTCHA_DEFAULT_LOG_QUOTA":                true,
	"GOTCHA_DEFAULT_METRIC_QUOTA":             true,
	"GOTCHA_DEFAULT_PROFILE_QUOTA":            true,
	"GOTCHA_DEFAULT_TRANSACTION_QUOTA":        true,
	"GOTCHA_DEPENDENCY_SETTLE_SECONDS":        true,
	"GOTCHA_DEPLOY_RETENTION_DAYS":            true,
	"GOTCHA_DIST_DIR":                         true,
	"GOTCHA_DIST_RATE_PER_MIN":                true,
	"GOTCHA_EDITION":                          true,
	"GOTCHA_ESCALATION_INTERVAL_SECONDS":      true,
	"GOTCHA_EVALUATORS_ENABLED":               true,
	"GOTCHA_EVENT_RETENTION_DAYS":             true,
	"GOTCHA_EXPORT_DIR":                       true,
	"GOTCHA_EXPORT_DISK_BUDGET_BYTES":         true,
	"GOTCHA_EXPORT_MAX_BYTES":                 true,
	"GOTCHA_EXPORT_MAX_ROWS":                  true,
	"GOTCHA_EXPORT_RETENTION_HOURS":           true,
	"GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED": true,
	"GOTCHA_HOST_EVAL_INTERVAL_SECONDS":       true,
	"GOTCHA_HSTS_ENABLED":                     true,
	"GOTCHA_HSTS_INCLUDE_SUBDOMAINS":          true,
	"GOTCHA_HSTS_MAX_AGE_SECONDS":             true,
	"GOTCHA_HSTS_PRELOAD":                     true,
	"GOTCHA_INCIDENT_RETENTION_DAYS":          true,
	"GOTCHA_INGEST_RATE_PER_SEC":              true,
	"GOTCHA_LISTEN_ADDR":                      true,
	"GOTCHA_LOCALE":                           true,
	"GOTCHA_LOGGING_FORMAT":                   true,
	"GOTCHA_LOGGING_LEVEL":                    true,
	"GOTCHA_LOG_RETENTION_DAYS":               true,
	"GOTCHA_MAX_EVENT_BYTES":                  true,
	"GOTCHA_MAX_INGEST_QUEUE_BYTES":           true,
	"GOTCHA_MAX_WRITER_BUFFER_BYTES":          true,
	"GOTCHA_METRIC_EVAL_INTERVAL_SECONDS":     true,
	"GOTCHA_METRIC_RETENTION_DAYS":            true,
	"GOTCHA_NOTIFY_CONCURRENCY":               true,
	"GOTCHA_OIDC_CLIENT_ID":                   true,
	"GOTCHA_OIDC_CLIENT_SECRET":               true,
	"GOTCHA_OIDC_DISPLAY_NAME":                true,
	"GOTCHA_OIDC_ENABLED":                     true,
	"GOTCHA_OIDC_ISSUER":                      true,
	"GOTCHA_OIDC_SCOPES":                      true,
	"GOTCHA_OUTBOX_RETENTION_DAYS":            true,
	"GOTCHA_PG_DSN":                           true,
	"GOTCHA_PROBE_KEY":                        true,
	"GOTCHA_PROBE_SERVER_URL":                 true,
	"GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS":    true,
	"GOTCHA_PROFILE_RETENTION_DAYS":           true,
	"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS":    true,
	"GOTCHA_REGISTRATION_MODE":                true,
	"GOTCHA_SCRUB_DENY_KEYS":                  true,
	"GOTCHA_SCRUB_EMAIL":                      true,
	"GOTCHA_SCRUB_FREETEXT":                   true,
	"GOTCHA_SCRUB_IP":                         true,
	"GOTCHA_SCRUB_KEEP_KEYS":                  true,
	"GOTCHA_SECRET_KEY":                       true,
	"GOTCHA_SECRET_KEY_ALLOW_INSECURE":        true,
	"GOTCHA_SECRET_KEY_PREV":                  true,
	"GOTCHA_SLO_EVAL_INTERVAL_SECONDS":        true,
	"GOTCHA_SMTP_FROM":                        true,
	"GOTCHA_SMTP_HOST":                        true,
	"GOTCHA_SMTP_PASSWORD":                    true,
	"GOTCHA_SMTP_PORT":                        true,
	"GOTCHA_SMTP_USER":                        true,
	"GOTCHA_SPAN_RETENTION_DAYS":              true,
	"GOTCHA_SSRF_ALLOW_PRIVATE":               true,
	"GOTCHA_SSRF_ALLOW_PRIVATE_OIDC":          true,
	"GOTCHA_SSRF_ALLOW_PRIVATE_TELEGRAM":      true,
	"GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME":        true,
	"GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK":       true,
	"GOTCHA_TELEGRAM_API_BASE":                true,
	"GOTCHA_TRUSTED_PROXIES":                  true,
	"GOTCHA_TRUSTED_RECIPIENTS":               true,
	"GOTCHA_UPTIME_CONCURRENCY":               true,
	"GOTCHA_UPTIME_LOCAL_REGION":              true,
	"GOTCHA_VK_CLIENT_ID":                     true,
	"GOTCHA_VK_CLIENT_SECRET":                 true,
	"GOTCHA_VK_ENABLED":                       true,
	"GOTCHA_YANDEX_CLIENT_ID":                 true,
	"GOTCHA_YANDEX_CLIENT_SECRET":             true,
	"GOTCHA_YANDEX_ENABLED":                   true,

	// internal/agent/config.go — gotcha-agent
	"GOTCHA_AGENT_CA_CERT":                  true,
	"GOTCHA_AGENT_ENDPOINT":                 true,
	"GOTCHA_AGENT_ENVIRONMENT":              true,
	"GOTCHA_AGENT_HOSTNAME":                 true,
	"GOTCHA_AGENT_INGEST_KEY":               true,
	"GOTCHA_AGENT_INTERVAL_SECONDS":         true,
	"GOTCHA_AGENT_ROLE":                     true,
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": true,
}
