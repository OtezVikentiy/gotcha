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

// TestEnvcontractRenamedComplete — envcontract.Renamed держит РОВНО десять
// пар и покрывает весь список из блока `### Changed` CHANGELOG (волна
// контрактной уборки v0.23.0). Тест на полноту: если карту в будущем
// случайно урежут (например, забудут добавить одиннадцатое переименование
// или потеряют одну пару при рефакторинге), этот тест укажет на
// расхождение с документированным контрактом, а не только на количество.
// want прописан буквально и сознательно НЕ переиспользует envcontract.Renamed
// — иначе тест сверял бы карту саму с собой и не заметил бы никакой порчи.
func TestEnvcontractRenamedComplete(t *testing.T) {
	want := map[string]string{
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
	if len(envcontract.Renamed) != 10 {
		t.Errorf("len(envcontract.Renamed) = %d, want 10", len(envcontract.Renamed))
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

// TestRenamedEnvVarNewNameChecksComplete — renamedEnvVarNewNameChecks
// (cmd/gotcha/config_test.go, регрессионные подтесты «новое имя применяется
// как обычно») обязана содержать РОВНО те новые имена, что есть среди
// значений envcontract.Renamed — ни лишних, ни пропущенных. Таблица и карта
// живут в разных местах не просто рядом: без этой сверки одиннадцатая пара,
// добавленная в envcontract.Renamed, тихо осталась бы без регрессионного
// подтеста в TestLoadConfigRenamedEnvVarNewNameStillApplies — ровно то же
// расхождение таблицы и истины, которое уже один раз привело к тому, что
// девять из десяти строк таблицы не вызывались никогда (таблица заявляла
// покрытие, которого не было). Сравнение только НОВЫХ имён — они не под
// сторожем TestNoRenamedEnvVarNames, поэтому написать их буквально можно
// в любом файле cmd/gotcha, в том числе в config_test.go.
func TestRenamedEnvVarNewNameChecksComplete(t *testing.T) {
	wantNewNames := make(map[string]bool, len(envcontract.Renamed))
	for _, newName := range envcontract.Renamed {
		wantNewNames[newName] = true
	}
	for newName := range renamedEnvVarNewNameChecks {
		if !wantNewNames[newName] {
			t.Errorf("renamedEnvVarNewNameChecks содержит лишнюю запись %s — среди значений envcontract.Renamed такого нет", newName)
		}
	}
	for newName := range wantNewNames {
		if _, ok := renamedEnvVarNewNameChecks[newName]; !ok {
			t.Errorf("renamedEnvVarNewNameChecks не хватает записи для %s (есть в envcontract.Renamed, но без регрессионного подтеста)", newName)
		}
	}
}
