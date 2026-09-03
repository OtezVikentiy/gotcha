package agent

import (
	"strings"
	"testing"
)

// TestCheckUnknownAgentEnvVarsAcceptsKnownVars — регрессия: сама проверка не
// должна отказывать на легитимном имени, ради которого её и завели.
func TestCheckUnknownAgentEnvVarsAcceptsKnownVars(t *testing.T) {
	if err := checkUnknownAgentEnvVars(environFrom(
		"GOTCHA_AGENT_ENDPOINT=https://g.example",
		"GOTCHA_AGENT_INGEST_KEY=pk",
		"GOTCHA_AGENT_INTERVAL_SECONDS=30",
	)); err != nil {
		t.Errorf("checkUnknownAgentEnvVars: %v, want nil для известных агентских имён", err)
	}
}

// TestCheckUnknownAgentEnvVarsIgnoresForeignNamespace — прод-сценарий из
// брифа: агент и сервер штатно делят один хост и общий `.env`. Серверная
// переменная (даже опечатанная) — легитимный сосед по файлу, агент её
// никогда не читал и не обязан знать её имя.
func TestCheckUnknownAgentEnvVarsIgnoresForeignNamespace(t *testing.T) {
	if err := checkUnknownAgentEnvVars(environFrom(
		"GOTCHA_PG_DSN=postgres://x",
		"GOTCHA_HSTS_ENABLE=false", // опечатка в СЕРВЕРНОМ имени — не наша забота
	)); err != nil {
		t.Errorf("checkUnknownAgentEnvVars: %v, want nil для чужого неймспейса", err)
	}
}

// TestCheckUnknownAgentEnvVarsRejectsTypoInOwnNamespace — I4 (финальное
// ревью): опечатка ВНУТРИ своего же префикса GOTCHA_AGENT_ обязана ронять
// старт — до этой правки install.sh --check подтверждал такой конфиг как
// "config OK", а интервал молча уходил на дефолт.
func TestCheckUnknownAgentEnvVarsRejectsTypoInOwnNamespace(t *testing.T) {
	err := checkUnknownAgentEnvVars(environFrom("GOTCHA_AGENT_INTERVAL_SECOND=30"))
	if err == nil {
		t.Fatal("checkUnknownAgentEnvVars: want ошибку на GOTCHA_AGENT_INTERVAL_SECOND, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_INTERVAL_SECOND") {
		t.Errorf("err = %q, want упоминание неизвестного имени GOTCHA_AGENT_INTERVAL_SECOND", err)
	}
}

// TestCheckUnknownAgentEnvVarsRejectsTypoEvenWithEmptyValue — R3
// (повторное ревью): значение проверяемого имени не смотрится — тот же
// контракт, что у серверного checkUnknownEnvVars (значение там вообще не
// читается). GOTCHA_AGENT_INTERVAL_SECOND (без завершающего S) никогда не
// было легитимным именем ни под каким значением — пустое не должно спасать
// опечатку от отказа так же, как не спасает непустое.
func TestCheckUnknownAgentEnvVarsRejectsTypoEvenWithEmptyValue(t *testing.T) {
	err := checkUnknownAgentEnvVars(environFrom("GOTCHA_AGENT_INTERVAL_SECOND="))
	if err == nil {
		t.Fatal("checkUnknownAgentEnvVars: want ошибку на GOTCHA_AGENT_INTERVAL_SECOND даже с пустым значением, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_INTERVAL_SECOND") {
		t.Errorf("err = %q, want упоминание GOTCHA_AGENT_INTERVAL_SECOND", err)
	}
}

// TestCheckUnknownAgentEnvVarsIgnoresEmptyRenamedName — declared-but-unset:
// docker-compose штатно прокидывает объявленные, но не заданные переменные
// пустой строкой (см. TestLoadConfigRenamedEnvVarEmptyDoesNotFailStart).
// В отличие от опечатки выше, СТАРОЕ ИМЯ, которое когда-то было легитимным
// (envcontract.Renamed), с пустым значением — не повод отказывать старту:
// CheckRenamedScoped выше по той же причине пропускает его тем же
// правилом, и повторный отказ здесь под другим (менее точным) текстом был
// бы избыточен.
func TestCheckUnknownAgentEnvVarsIgnoresEmptyRenamedName(t *testing.T) {
	old := sortedAgentOwnedOldNames()[0]
	if err := checkUnknownAgentEnvVars(environFrom(old + "=")); err != nil {
		t.Errorf("checkUnknownAgentEnvVars(%s=\"\"): %v, want nil (старое имя, пустое — declared-but-unset)", old, err)
	}
}

// TestCheckUnknownAgentEnvVarsListsAllFindingsSorted — несколько находок
// разом, в детерминированном порядке — не в порядке environ(), который
// ничего не гарантирует.
func TestCheckUnknownAgentEnvVarsListsAllFindingsSorted(t *testing.T) {
	err := checkUnknownAgentEnvVars(environFrom(
		"GOTCHA_AGENT_ZZZ_MADE_UP=1",
		"GOTCHA_AGENT_AAA_MADE_UP=1",
	))
	if err == nil {
		t.Fatal("checkUnknownAgentEnvVars: want ошибку на двух неизвестных именах, получили nil")
	}
	msg := err.Error()
	posA := strings.Index(msg, "GOTCHA_AGENT_AAA_MADE_UP")
	posZ := strings.Index(msg, "GOTCHA_AGENT_ZZZ_MADE_UP")
	if posA < 0 || posZ < 0 {
		t.Fatalf("err = %q, want упоминание обоих неизвестных имён", msg)
	}
	if posA > posZ {
		t.Errorf("err = %q, want GOTCHA_AGENT_AAA_MADE_UP раньше GOTCHA_AGENT_ZZZ_MADE_UP (алфавитный порядок)", msg)
	}
}

// TestLoadConfigRejectsUnknownOwnNamespaceVar — тот же случай сквозь
// LoadConfig целиком, не только checkUnknownAgentEnvVars напрямую: реальный
// прод-путь — install.sh --check зовёт LoadConfig, не внутреннюю функцию.
func TestLoadConfigRejectsUnknownOwnNamespaceVar(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":        "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY":      "pk",
		"GOTCHA_AGENT_INTERVAL_SECOND": "30",
	}))
	if err == nil {
		t.Fatal("LoadConfig: want ошибку на GOTCHA_AGENT_INTERVAL_SECOND, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_INTERVAL_SECOND") {
		t.Errorf("err = %q, want упоминание GOTCHA_AGENT_INTERVAL_SECOND", err)
	}
}

// TestLoadConfigRejectsUnknownOwnNamespaceVarEvenWithEmptyValue — R3
// (повторное ревью) сквозь LoadConfig целиком: GOTCHA_AGENT_TYPO= (пустое
// значение) отказывает старт так же, как непустое, — install.sh --check не
// должен подтверждать конфиг с настоящей опечаткой в имени только потому,
// что значение оказалось пустым.
func TestLoadConfigRejectsUnknownOwnNamespaceVarEvenWithEmptyValue(t *testing.T) {
	vars := map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
	}
	getenv, environ := env(vars)
	environWithTypo := func() []string {
		return append(environ(), "GOTCHA_AGENT_TYPO=")
	}
	_, err := LoadConfig(getenv, environWithTypo)
	if err == nil {
		t.Fatal("LoadConfig: want ошибку на GOTCHA_AGENT_TYPO= (пустое значение), получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_TYPO") {
		t.Errorf("err = %q, want упоминание GOTCHA_AGENT_TYPO", err)
	}
}

// TestLoadConfigAcceptsForeignServerVar — прод-сценарий из брифа сквозь
// LoadConfig целиком: чужая GOTCHA_PG_DSN в окружении агента — не отказ.
func TestLoadConfigAcceptsForeignServerVar(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"GOTCHA_AGENT_ENDPOINT":   "https://g.example",
		"GOTCHA_AGENT_INGEST_KEY": "pk",
		"GOTCHA_PG_DSN":           "postgres://u:p@pg:5432/g",
	}))
	if err != nil {
		t.Fatalf("LoadConfig с чужой GOTCHA_PG_DSN: %v, want nil", err)
	}
	if cfg.Endpoint != "https://g.example" {
		t.Errorf("Endpoint = %q, want https://g.example", cfg.Endpoint)
	}
}
