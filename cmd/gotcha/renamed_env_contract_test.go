package main

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Файл существует ОТДЕЛЬНО от config_test.go намеренно: это единственное
// место в cmd/gotcha, где старые имена envcontract.Renamed пишутся буквально
// (справочник для независимой сверки с CHANGELOG ниже), поэтому именно оно —
// точечное исключение internal/guards/renamed_env_vars_test.go
// (TestNoRenamedEnvVarNames), а не config_test.go целиком. config_test.go
// растёт с каждой фичей конфига и не должен становиться слепой зоной
// сторожа из-за соседства с этим справочником — свои поведенческие тесты
// отказа старта он берёт из envcontract.Renamed динамически, без единого
// литерала старого имени (см. sortedRenamedOldNames в config_test.go).

// TestEnvcontractRenamedComplete — envcontract.Renamed держит РОВНО
// тридцать пар (десять из волны контрактной уборки v0.23.0, семнадцать
// серверных из волны заморозки контракта перед 1.0 плюс три агентские из
// той же волны) и покрывает весь список, документированный в CHANGELOG.
// Тест на полноту: если карту в
// будущем случайно урежут (например, забудут добавить очередное
// переименование или потеряют одну пару при рефакторинге), этот тест
// укажет на расхождение с документированным контрактом, а не только на
// количество. want прописан буквально и сознательно НЕ переиспользует
// envcontract.Renamed — иначе тест сверял бы карту саму с собой и не
// заметил бы никакой порчи.
func TestEnvcontractRenamedComplete(t *testing.T) {
	want := map[string]string{
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
		"GOTCHA_AGENT_INTERVAL":           "GOTCHA_AGENT_INTERVAL_SECONDS",
		"GOTCHA_AGENT_KEY":                "GOTCHA_AGENT_INGEST_KEY",
		"GOTCHA_AGENT_TLS_SKIP_VERIFY":    "GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY",
	}
	if len(envcontract.Renamed) != 30 {
		t.Errorf("len(envcontract.Renamed) = %d, want 30", len(envcontract.Renamed))
	}
	for old, newName := range want {
		got, ok := envcontract.Renamed[old]
		if !ok {
			t.Errorf("envcontract.Renamed отсутствует пара для %s (want %s)", old, newName)
			continue
		}
		if got != newName {
			t.Errorf("envcontract.Renamed[%s] = %s, want %s", old, got, newName)
		}
	}
	for old := range envcontract.Renamed {
		if _, ok := want[old]; !ok {
			t.Errorf("envcontract.Renamed содержит лишнюю пару %s, не документированную в CHANGELOG", old)
		}
	}
}

// agentOwnedRenamedNewNames — новые имена envcontract.Renamed, которые
// читает internal/agent (отдельный бинарь gotcha-agent), а НЕ cmd/gotcha:
// у Config здесь нет и не может быть поля под GOTCHA_AGENT_INGEST_KEY/
// GOTCHA_AGENT_INTERVAL_SECONDS/GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY,
// поэтому регрессионный подтест «новое имя применяется как обычно» для них
// живёт в internal/agent/config_test.go (agentRenamedEnvVarNewNameChecks +
// TestLoadConfigRenamedEnvVarNewNameStillApplies того пакета), а не здесь.
// Выведено ИЗ envcontract.AgentOwned (internal/envcontract/renamed.go) —
// единственного источника, не хардкод: добавление агентской пары в
// AgentOwned меняет и это множество, без правки cmd/gotcha (ops-review E3
// T8 круг 1 — раньше список дублировался руками в двух местах).
func agentOwnedRenamedNewNames() map[string]bool {
	m := make(map[string]bool, len(envcontract.AgentOwned))
	for _, old := range envcontract.AgentOwned {
		m[envcontract.Renamed[old]] = true
	}
	return m
}

// TestRenamedEnvVarNewNameChecksComplete — renamedEnvVarNewNameChecks
// (cmd/gotcha/config_test.go, регрессионные подтесты «новое имя применяется
// как обычно») обязана содержать РОВНО те новые имена, что есть среди
// значений envcontract.Renamed И принадлежат cmd/gotcha (не входят в
// agentOwnedRenamedNewNames выше) — ни лишних, ни пропущенных. Таблица и
// карта живут в разных местах не просто рядом: без этой сверки
// одиннадцатая пара, добавленная в envcontract.Renamed, тихо осталась бы
// без регрессионного подтеста в TestLoadConfigRenamedEnvVarNewNameStillApplies
// — ровно то же расхождение таблицы и истины, которое уже один раз привело
// к тому, что девять из десяти строк таблицы не вызывались никогда
// (таблица заявляла покрытие, которого не было). Сравнение только НОВЫХ
// имён — они не под сторожем TestNoRenamedEnvVarNames, поэтому написать их
// буквально можно в любом файле cmd/gotcha, в том числе в config_test.go.
func TestRenamedEnvVarNewNameChecksComplete(t *testing.T) {
	agentOwned := agentOwnedRenamedNewNames()
	wantNewNames := make(map[string]bool, len(envcontract.Renamed))
	for _, newName := range envcontract.Renamed {
		wantNewNames[newName] = true
	}
	for newName := range renamedEnvVarNewNameChecks {
		if agentOwned[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит %s — она агентская (envcontract.AgentOwned), регрессия для неё живёт в internal/agent/config_test.go", newName)
		}
		if !wantNewNames[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит лишнюю запись %s — среди значений envcontract.Renamed такого нет", newName)
		}
	}
	for newName := range wantNewNames {
		if agentOwned[newName] {
			continue
		}
		if _, ok := renamedEnvVarNewNameChecks[newName]; !ok {
			t.Errorf("renamedEnvVarNewNameChecks не хватает записи для %s (есть в envcontract.Renamed, но без регрессионного подтеста)", newName)
		}
	}
}
