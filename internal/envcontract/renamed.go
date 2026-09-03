// Package envcontract — единая истина о контракте переменных окружения между
// релизами продукта: сейчас в нём ровно один факт, карта переименований, но
// имя пакета сознательно не «renamedenv» — сюда же со временем ляжет любой
// другой факт о совместимости env между версиями (например, будущие
// удаления без замены), если он появится.
package envcontract

// Renamed — старые имена переменных окружения → новые, накопительно по всем
// волнам переименования. cmd/gotcha/config.go и internal/agent/config.go
// старые имена уже не читают нигде — без явного отказа старта апгрейд
// инстанса с непровённым `.env` не падал бы, а тихо подменял значение
// оператора дефолтом (см. cmd/gotcha/config.go, врезка checkRenamedEnvVars).
// Политика — жёсткий fail-fast без мягкой депрекации: однажды попавшее сюда
// старое имя остаётся под отказом старта навсегда, карта только растёт.
// Ниже — две волны: контрактная уборка v0.23.0 (см. блок `### Changed` в
// CHANGELOG «Ten environment variables have been renamed») и заморозка
// контракта перед 1.0 (см. CHANGELOG, «Семнадцать серверных переменных
// переименованы»).
//
// Единственная копия карты в дереве: internal/guards/renamed_env_vars_test.go
// импортирует именно её, а не держит собственный список — иначе два места
// расходятся при следующем переименовании, и сторож начинает проверять не то,
// что реально проверяет отказ старта.
var Renamed = map[string]string{
	// v0.23.0
	"GOTCHA_METRIC_EVAL_INTERVAL":    "GOTCHA_METRIC_EVAL_INTERVAL_SECONDS",
	"GOTCHA_PROFILE_EVAL_INTERVAL":   "GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS",
	"GOTCHA_HOST_EVAL_INTERVAL":      "GOTCHA_HOST_EVAL_INTERVAL_SECONDS",
	"GOTCHA_SLO_EVAL_INTERVAL":       "GOTCHA_SLO_EVAL_INTERVAL_SECONDS",
	"GOTCHA_ESCALATION_INTERVAL":     "GOTCHA_ESCALATION_INTERVAL_SECONDS",
	"GOTCHA_RETENTION_DAYS":          "GOTCHA_EVENT_RETENTION_DAYS",
	"GOTCHA_SERVER_URL":              "GOTCHA_PROBE_SERVER_URL",
	"GOTCHA_INGEST_RATE_LIMIT":       "GOTCHA_INGEST_RATE_PER_SEC",
	"GOTCHA_AGENT_DIST_DIR":          "GOTCHA_DIST_DIR",
	"GOTCHA_AGENT_DIST_RATE_PER_MIN": "GOTCHA_DIST_RATE_PER_MIN",
	// E3, заморозка контракта
	"GOTCHA_ADDR":                     "GOTCHA_LISTEN_ADDR",
	"GOTCHA_LOG_LEVEL":                "GOTCHA_LOGGING_LEVEL",
	"GOTCHA_LOG_FORMAT":               "GOTCHA_LOGGING_FORMAT",
	"GOTCHA_LOCAL_REGION":             "GOTCHA_UPTIME_LOCAL_REGION",
	"GOTCHA_REGISTRATION":             "GOTCHA_REGISTRATION_MODE",
	"GOTCHA_EXPORT_TTL_HOURS":         "GOTCHA_EXPORT_RETENTION_HOURS",
	"GOTCHA_SCRUB_KEYS":               "GOTCHA_SCRUB_DENY_KEYS",
	"GOTCHA_SCRUB_ALLOW_KEYS":         "GOTCHA_SCRUB_KEEP_KEYS",
	"GOTCHA_RUN_EVALUATORS":           "GOTCHA_EVALUATORS_ENABLED",
	"GOTCHA_AUTO_MIGRATE":             "GOTCHA_AUTO_MIGRATE_ENABLED",
	"GOTCHA_ALLOW_INSECURE_SECRET":    "GOTCHA_SECRET_KEY_ALLOW_INSECURE",
	"GOTCHA_MAX_BUFFER_BYTES":         "GOTCHA_MAX_WRITER_BUFFER_BYTES",
	"GOTCHA_MAX_QUEUE_BYTES":          "GOTCHA_MAX_INGEST_QUEUE_BYTES",
	"GOTCHA_PROBE_TOKEN":              "GOTCHA_PROBE_KEY",
	"GOTCHA_EXTERNAL_CHANNEL_DETAILS": "GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED",
	"GOTCHA_OIDC_NAME":                "GOTCHA_OIDC_DISPLAY_NAME",
	"GOTCHA_PURGE_RECONCILE_HOURS":    "GOTCHA_PROJECT_PURGE_RECONCILE_HOURS",
	// E3, заморозка контракта — контракт агента
	"GOTCHA_AGENT_INTERVAL":        "GOTCHA_AGENT_INTERVAL_SECONDS",
	"GOTCHA_AGENT_KEY":             "GOTCHA_AGENT_INGEST_KEY",
	"GOTCHA_AGENT_TLS_SKIP_VERIFY": "GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY",
}
