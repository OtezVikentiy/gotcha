package main

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Файл — точечное исключение из TestNoRenamedEnvVarNames (internal/guards):
// единственное место, где старые имена envcontract.Renamed пишутся буквально.

// want прописан буквально, не через envcontract.Renamed — иначе тест сверял
// бы карту саму с собой и не заметил бы порчи.
func TestEnvcontractRenamedComplete(t *testing.T) {
	want := map[string]string{
		"GOTCHA_METRIC_EVAL_INTERVAL":     "GOTCHA_METRIC_EVAL_INTERVAL_SECONDS",
		"GOTCHA_PROFILE_EVAL_INTERVAL":    "GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS",
		"GOTCHA_HOST_EVAL_INTERVAL":       "GOTCHA_HOST_EVAL_INTERVAL_SECONDS",
		"GOTCHA_SLO_EVAL_INTERVAL":        "GOTCHA_SLO_EVAL_INTERVAL_SECONDS",
		"GOTCHA_ESCALATION_INTERVAL":      "GOTCHA_ESCALATION_INTERVAL_SECONDS",
		"GOTCHA_RETENTION_DAYS":           "GOTCHA_EVENT_RETENTION_DAYS",
		"GOTCHA_SERVER_URL":               "GOTCHA_PROBE_SERVER_URL",
		"GOTCHA_INGEST_RATE_LIMIT":        "GOTCHA_INGEST_RATE_PER_SEC",
		"GOTCHA_AGENT_DIST_DIR":           "GOTCHA_DIST_DIR",
		"GOTCHA_AGENT_DIST_RATE_PER_MIN":  "GOTCHA_DIST_RATE_PER_MIN",
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
		"GOTCHA_PG_PASSWORD":              "GOTCHA_COMPOSE_PG_PASSWORD",
		"GOTCHA_CH_PASSWORD":              "GOTCHA_COMPOSE_CH_PASSWORD",
		"GOTCHA_PG_MEM_LIMIT":             "GOTCHA_COMPOSE_PG_MEM_LIMIT",
		"GOTCHA_CH_MEM_LIMIT":             "GOTCHA_COMPOSE_CH_MEM_LIMIT",
		"GOTCHA_MEM_LIMIT":                "GOTCHA_COMPOSE_MEM_LIMIT",
		"GOTCHA_NET_MTU":                  "GOTCHA_COMPOSE_NET_MTU",
		"GOTCHA_PORT":                     "GOTCHA_COMPOSE_PORT",
		"GOTCHA_BIND":                     "GOTCHA_COMPOSE_BIND",
		"GOTCHA_VERSION":                  "GOTCHA_BUILD_VERSION",
		"GOTCHA_COMMIT":                   "GOTCHA_BUILD_COMMIT",
		"GOTCHA_DATE":                     "GOTCHA_BUILD_DATE",
	}
	if len(envcontract.Renamed) != 41 {
		t.Errorf("len(envcontract.Renamed) = %d, want 41", len(envcontract.Renamed))
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

// Пары, которые читает internal/agent, а не cmd/gotcha — Config здесь не
// имеет для них поля, регрессия «новое имя работает» живёт в internal/agent.
func agentOwnedRenamedNewNames() map[string]bool {
	m := make(map[string]bool, len(envcontract.AgentOwned))
	for _, old := range envcontract.AgentOwned {
		m[envcontract.Renamed[old]] = true
	}
	return m
}

// Пары compose/build (GOTCHA_COMPOSE_*/GOTCHA_BUILD_*) — их не читает никакой
// Go-код, подставляют только Docker Compose и Makefile.
func infraOwnedRenamedNewNames() map[string]bool {
	m := make(map[string]bool, len(envcontract.InfraOwned))
	for _, old := range envcontract.InfraOwned {
		m[envcontract.Renamed[old]] = true
	}
	return m
}

// Сверяет renamedEnvVarNewNameChecks с envcontract.Renamed построчно — без
// неё новая пара тихо осталась бы без регрессионного подтеста.
func TestRenamedEnvVarNewNameChecksComplete(t *testing.T) {
	agentOwned := agentOwnedRenamedNewNames()
	infraOwned := infraOwnedRenamedNewNames()
	wantNewNames := make(map[string]bool, len(envcontract.Renamed))
	for _, newName := range envcontract.Renamed {
		wantNewNames[newName] = true
	}
	for newName := range renamedEnvVarNewNameChecks {
		if agentOwned[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит %s — она агентская (envcontract.AgentOwned), регрессия для неё живёт в internal/agent/config_test.go", newName)
		}
		if infraOwned[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит %s — она compose/build (envcontract.InfraOwned), ни один Go-код её не читает, регрессионному подтесту взяться неоткуда", newName)
		}
		if !wantNewNames[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит лишнюю запись %s — среди значений envcontract.Renamed такого нет", newName)
		}
	}
	for newName := range wantNewNames {
		if agentOwned[newName] || infraOwned[newName] {
			continue
		}
		if _, ok := renamedEnvVarNewNameChecks[newName]; !ok {
			t.Errorf("renamedEnvVarNewNameChecks не хватает записи для %s (есть в envcontract.Renamed, но без регрессионного подтеста)", newName)
		}
	}
}
