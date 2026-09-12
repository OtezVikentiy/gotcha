package agent

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

func TestCheckUnknownAgentEnvVarsAcceptsKnownVars(t *testing.T) {
	if err := checkUnknownAgentEnvVars(environFrom(
		"GOTCHA_AGENT_ENDPOINT=https://g.example",
		"GOTCHA_AGENT_INGEST_KEY=pk",
		"GOTCHA_AGENT_INTERVAL_SECONDS=30",
	)); err != nil {
		t.Errorf("checkUnknownAgentEnvVars: %v, want nil для известных агентских имён", err)
	}
}

func TestCheckUnknownAgentEnvVarsIgnoresForeignNamespace(t *testing.T) {
	if err := checkUnknownAgentEnvVars(environFrom(
		"GOTCHA_PG_DSN=postgres://x",
		"GOTCHA_HSTS_ENABLE=false", // опечатка в СЕРВЕРНОМ имени — не наша забота
	)); err != nil {
		t.Errorf("checkUnknownAgentEnvVars: %v, want nil для чужого неймспейса", err)
	}
}

func TestCheckUnknownAgentEnvVarsRejectsTypoInOwnNamespace(t *testing.T) {
	err := checkUnknownAgentEnvVars(environFrom("GOTCHA_AGENT_INTERVAL_SECOND=30"))
	if err == nil {
		t.Fatal("checkUnknownAgentEnvVars: want ошибку на GOTCHA_AGENT_INTERVAL_SECOND, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_INTERVAL_SECOND") {
		t.Errorf("err = %q, want упоминание неизвестного имени GOTCHA_AGENT_INTERVAL_SECOND", err)
	}
}

func TestCheckUnknownAgentEnvVarsRejectsTypoEvenWithEmptyValue(t *testing.T) {
	err := checkUnknownAgentEnvVars(environFrom("GOTCHA_AGENT_INTERVAL_SECOND="))
	if err == nil {
		t.Fatal("checkUnknownAgentEnvVars: want ошибку на GOTCHA_AGENT_INTERVAL_SECOND даже с пустым значением, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_AGENT_INTERVAL_SECOND") {
		t.Errorf("err = %q, want упоминание GOTCHA_AGENT_INTERVAL_SECOND", err)
	}
}

func TestCheckUnknownAgentEnvVarsRejectsEmptyRenamedName(t *testing.T) {
	old := sortedAgentOwnedOldNames()[0]
	newName := envcontract.Renamed[old]
	err := checkUnknownAgentEnvVars(environFrom(old + "="))
	if err == nil {
		t.Fatalf("checkUnknownAgentEnvVars(%s=\"\"): want ошибку renamed, получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
}

func TestCheckUnknownAgentEnvVarsRejectsForeignRenamedNameUnderOwnPrefix(t *testing.T) {
	old := foreignRenamedNameUnderOwnPrefix(t)
	newName := envcontract.Renamed[old]
	err := checkUnknownAgentEnvVars(environFrom(old + "=/opt/x"))
	if err == nil {
		t.Fatalf("checkUnknownAgentEnvVars(%s=/opt/x): want ошибку renamed, получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
	if strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %q, want текст переименования, а не «unknown … typos»", err)
	}
}

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
