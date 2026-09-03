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
// оператора дефолтом (см. CheckRenamedAll/CheckRenamedScoped в check.go —
// общая реализация отказа для обеих сторон). Политика — жёсткий fail-fast
// без мягкой депрекации: однажды попавшее сюда старое имя остаётся под
// отказом старта минимум один мажорный релиз после добавления (для записей
// ниже это 2.0 — см. /docs/versioning, «Сроки текущего устаревшего»); внутри
// мажора карта только растёт.
// Ниже — три волны: контрактная уборка v0.23.0 (см. блок `### Changed` в
// CHANGELOG «Ten environment variables have been renamed»), заморозка
// контракта перед 1.0 (см. CHANGELOG, «Семнадцать серверных переменных
// переименованы») и, той же волной заморозки, неймспейс compose и сборки
// (см. CHANGELOG, «Переменные compose и сборки переименованы»).
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
	// E3, заморозка контракта — неймспейс compose и сборки: ни одно из
	// имён слева не было и не будет полем cmd/gotcha.Config — процесс их
	// не читает вовсе, читает только сам Docker Compose (подстановка
	// ${...} в docker-compose.yml/.small.yml) либо Makefile
	// (DOCKER_BUILD_ENV, build-args образа). Общий с серверными и
	// агентскими переменными неймспейс `GOTCHA_*` без масштабирующего
	// префикса означал, что оператор на Kubernetes/systemd, задавший,
	// скажем, GOTCHA_PG_PASSWORD напрямую, не получал ни эффекта (никто
	// её не читает), ни диагностики (имя валидно выглядит как продуктовая
	// переменная). Отказ старта работает и на них: docker-compose.yml
	// подключает `.env` целиком (`env_file`), так что устаревшее имя в
	// `.env` всё равно долетает до окружения процесса gotcha, хотя сам
	// процесс его не читает.
	"GOTCHA_PG_PASSWORD":  "GOTCHA_COMPOSE_PG_PASSWORD",
	"GOTCHA_CH_PASSWORD":  "GOTCHA_COMPOSE_CH_PASSWORD",
	"GOTCHA_PG_MEM_LIMIT": "GOTCHA_COMPOSE_PG_MEM_LIMIT",
	"GOTCHA_CH_MEM_LIMIT": "GOTCHA_COMPOSE_CH_MEM_LIMIT",
	"GOTCHA_MEM_LIMIT":    "GOTCHA_COMPOSE_MEM_LIMIT",
	"GOTCHA_NET_MTU":      "GOTCHA_COMPOSE_NET_MTU",
	"GOTCHA_PORT":         "GOTCHA_COMPOSE_PORT",
	"GOTCHA_BIND":         "GOTCHA_COMPOSE_BIND",
	"GOTCHA_VERSION":      "GOTCHA_BUILD_VERSION",
	"GOTCHA_COMMIT":       "GOTCHA_BUILD_COMMIT",
	"GOTCHA_DATE":         "GOTCHA_BUILD_DATE",
}

// AgentOwned — подмножество старых имён Renamed, за отказ на которых
// отвечает сам internal/agent (три пары контракта агента выше), а не
// cmd/gotcha. Единственный источник для CheckRenamedScoped со стороны
// агента (см. check.go) и для регрессионных тестов cmd/gotcha/internal/agent, которые
// иначе держали бы собственную копию этого списка. Решение владельца:
// агент проверяет ТОЛЬКО свои старые имена, а не весь реестр целиком
// (включая 27 серверных переменных, которые никогда не читает) — env-файл
// легитимно несёт чужие переменные, отказ на них не защита, а
// самоуправство. internal/envcontract/check_test.go
// проверяет, что каждое имя отсюда реально есть среди ключей Renamed и
// несёт префикс GOTCHA_AGENT_ — рассинхрон ловится тестом.
var AgentOwned = []string{
	"GOTCHA_AGENT_INTERVAL",
	"GOTCHA_AGENT_KEY",
	"GOTCHA_AGENT_TLS_SKIP_VERIFY",
}

// InfraOwned — подмножество старых имён Renamed, чьё новое имя не является
// (и не может стать) полем ни cmd/gotcha.Config, ни internal/agent.Config:
// одиннадцать переменных compose и сборки выше. Единственный источник для
// cmd/gotcha/renamed_env_contract_test.go (исключение из
// renamedEnvVarNewNameChecks — таблица держит только регрессии применения
// НОВОГО имени к полю Config, а этим именам применяться некуда) — по тому
// же образцу, каким AgentOwned исключает агентские имена оттуда же:
// рассинхрон между InfraOwned и Renamed ловит TestInfraOwnedSubsetOfRenamed
// в check_test.go, а не вторая ручная копия списка в cmd/gotcha.
//
// В отличие от AgentOwned, у элементов здесь нет общего префикса имени
// (compose-переменные унаследовали разнородные имена задолго до этой
// волны) — общее у них новое имя: префикс GOTCHA_COMPOSE_ или
// GOTCHA_BUILD_, это и проверяет TestInfraOwnedSubsetOfRenamed.
var InfraOwned = []string{
	"GOTCHA_PG_PASSWORD",
	"GOTCHA_CH_PASSWORD",
	"GOTCHA_PG_MEM_LIMIT",
	"GOTCHA_CH_MEM_LIMIT",
	"GOTCHA_MEM_LIMIT",
	"GOTCHA_NET_MTU",
	"GOTCHA_PORT",
	"GOTCHA_BIND",
	"GOTCHA_VERSION",
	"GOTCHA_COMMIT",
	"GOTCHA_DATE",
}
