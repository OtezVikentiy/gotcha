// Package envcontract — единая истина о контракте переменных окружения между
// релизами продукта: сейчас в нём ровно один факт, карта переименований, но
// имя пакета сознательно не «renamedenv» — сюда же со временем ляжет любой
// другой факт о совместимости env между версиями (например, будущие
// удаления без замены), если он появится.
package envcontract

// Renamed — старые имена переменных окружения (волна контрактной уборки
// v0.23.0, см. блок `### Changed` в CHANGELOG «Ten environment variables have
// been renamed») → новые. cmd/gotcha/config.go и internal/agent/config.go
// старые имена уже не читают нигде — без явного отказа старта апгрейд
// инстанса с непровённым `.env` не падал бы, а тихо подменял значение
// оператора дефолтом (см. cmd/gotcha/config.go, врезка checkRenamedEnvVars).
//
// Единственная копия карты в дереве: internal/guards/renamed_env_vars_test.go
// импортирует именно её, а не держит собственный список — иначе два места
// расходятся при следующем переименовании, и сторож начинает проверять не то,
// что реально проверяет отказ старта.
var Renamed = map[string]string{
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
}
