package main

import (
	"sort"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

func environFrom(kv ...string) func() []string {
	return func() []string { return kv }
}

func TestCheckUnknownEnvVarsAcceptsKnownServerVars(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom(
		"GOTCHA_BASE_URL=https://gotcha.example.com",
		"GOTCHA_HSTS_ENABLED=true",
		"GOTCHA_EVALUATORS_ENABLED=false",
	)); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для известных серверных имён", err)
	}
}

func TestCheckUnknownEnvVarsAcceptsAgentVarsOnServer(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("GOTCHA_AGENT_INGEST_KEY=some-ingest-key")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для GOTCHA_AGENT_INGEST_KEY в окружении сервера", err)
	}
}

func TestCheckUnknownEnvVarsAcceptsComposeAndBuildPrefixes(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom(
		"GOTCHA_COMPOSE_PG_PASSWORD=secret",
		"GOTCHA_BUILD_VERSION=1.2.3",
	)); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для GOTCHA_COMPOSE_*/GOTCHA_BUILD_*", err)
	}
}

func TestCheckUnknownEnvVarsIgnoresNonGotchaVars(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("PATH=/usr/bin", "HOME=/root")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для переменных без префикса GOTCHA_", err)
	}
}

// Префикс "GOTCHA_" сравнивается регистрозависимо: "gotcha_lower" и
// "Gotcha_Port" не наши переменные вовсе, а не "неизвестные GOTCHA_*".
func TestCheckUnknownEnvVarsIsCasePreserving(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("gotcha_lower=1", "Gotcha_Port=1")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil — 'gotcha_lower'/'Gotcha_Port' не начинаются с 'GOTCHA_' регистрозависимо, это не наши переменные", err)
	}
}

func TestCheckUnknownEnvVarsIgnoresMalformedEntries(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("GOTCHA_WEIRD_NO_EQUALS_SIGN")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для записи без '='", err)
	}
}

func TestCheckUnknownEnvVarsRejectsTypo(t *testing.T) {
	err := checkUnknownEnvVars(environFrom("GOTCHA_HSTS_ENABLE=false"))
	if err == nil {
		t.Fatal("checkUnknownEnvVars: want ошибку на GOTCHA_HSTS_ENABLE, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_HSTS_ENABLE") {
		t.Errorf("err = %q, want упоминание неизвестного имени GOTCHA_HSTS_ENABLE", err)
	}
	if !strings.Contains(err.Error(), "GOTCHA_HSTS_ENABLED") {
		t.Errorf("err = %q, want подсказку GOTCHA_HSTS_ENABLED", err)
	}
}

func TestCheckUnknownEnvVarsListsAllFindingsSorted(t *testing.T) {
	err := checkUnknownEnvVars(environFrom(
		"GOTCHA_ZZZ_TOTALLY_MADE_UP=1",
		"GOTCHA_AAA_TOTALLY_MADE_UP=1",
	))
	if err == nil {
		t.Fatal("checkUnknownEnvVars: want ошибку на двух неизвестных именах, получили nil")
	}
	msg := err.Error()
	posA := strings.Index(msg, "GOTCHA_AAA_TOTALLY_MADE_UP")
	posZ := strings.Index(msg, "GOTCHA_ZZZ_TOTALLY_MADE_UP")
	if posA < 0 || posZ < 0 {
		t.Fatalf("err = %q, want упоминание обоих неизвестных имён", msg)
	}
	if posA > posZ {
		t.Errorf("err = %q, want GOTCHA_AAA_TOTALLY_MADE_UP раньше GOTCHA_ZZZ_TOTALLY_MADE_UP (алфавитный порядок)", msg)
	}
}

// Динамически, не литералом: TestNoRenamedEnvVarNames (internal/guards) не
// пускает старые имена литералом за пределы renamed_env_contract_test.go.
func sortedAgentOwnedOldNames() []string {
	names := make([]string, len(envcontract.AgentOwned))
	copy(names, envcontract.AgentOwned)
	sort.Strings(names)
	return names
}

// declared-but-unset не спасает переименованное имя, которое не читает уже
// никто — тот же контракт, что у internal/agent.checkUnknownAgentEnvVars.
func TestCheckUnknownEnvVarsRejectsEmptyRenamedName(t *testing.T) {
	old := sortedAgentOwnedOldNames()[0]
	newName := envcontract.Renamed[old]
	err := checkUnknownEnvVars(environFrom(old + "="))
	if err == nil {
		t.Fatalf("checkUnknownEnvVars(%s=\"\"): want ошибку renamed, получили nil", old)
	}
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want упоминание старого %s и нового %s имени", err, old, newName)
	}
	if strings.Contains(err.Error(), "typo") {
		t.Errorf("err = %q, want текст переименования, а не «unknown … typos» — это не опечатка, а известное старое имя", err)
	}
}

// Старое имя не входит в envcontract.Known — без верного порядка вызовов оно
// попало бы под checkUnknownEnvVars как обычная опечатка.
func TestLoadConfigCheckedOrderRenamedBeforeUnknown(t *testing.T) {
	old := sortedRenamedOldNames()[0]
	newName := envcontract.Renamed[old]

	_, err := loadConfigChecked(getenvFrom(map[string]string{old: "some-value"}), environFrom(old+"=some-value"), nil)
	if err == nil {
		t.Fatalf("loadConfigChecked: want ошибку на устаревшем %s, получили nil", old)
	}
	if !strings.Contains(err.Error(), "renamed to") || !strings.Contains(err.Error(), newName) {
		t.Errorf("err = %q, want точный ответ CheckRenamedAll (упоминание %q и \"renamed to\"), а не догадку checkUnknownEnvVars", err, newName)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("err = %q, want ответ CheckRenamedAll, а не подсказку checkUnknownEnvVars — проверки вызваны не в том порядке", err)
	}
}

func TestLoadConfigCheckedPropagatesUnknownAfterRenamedPasses(t *testing.T) {
	_, err := loadConfigChecked(getenvFrom(nil), environFrom("GOTCHA_HSTS_ENABLE=false"), nil)
	if err == nil {
		t.Fatal("loadConfigChecked: want ошибку на GOTCHA_HSTS_ENABLE, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_HSTS_ENABLE") {
		t.Errorf("err = %q, want упоминание GOTCHA_HSTS_ENABLE", err)
	}
}

func TestLoadConfigCheckedSucceedsOnCleanEnv(t *testing.T) {
	cfg, err := loadConfigChecked(getenvFrom(nil), environFrom(), nil)
	if err != nil {
		t.Fatalf("loadConfigChecked: %v, want nil на чистом окружении", err)
	}
	if cfg.Mode != "all" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "all")
	}
}

var levenshteinDistanceCases = []struct {
	a, b string
	want int
}{
	{"", "", 0},
	{"", "abc", 3},
	{"abc", "", 3},
	{"abc", "abc", 0},
	{"abc", "abd", 1},
	{"kitten", "sitting", 3},
	{"GOTCHA_HSTS_ENABLE", "GOTCHA_HSTS_ENABLED", 1},
	{"GOTCHA_HSTS_ENABLEDXX", "GOTCHA_HSTS_ENABLED", 2},
	{"GOTCHA_HSTS_ENABLEDXXX", "GOTCHA_HSTS_ENABLED", 3},
}

func TestLevenshteinDistance(t *testing.T) {
	for _, c := range levenshteinDistanceCases {
		if got := levenshteinDistance(c.a, c.b); got != c.want {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := levenshteinDistance(c.b, c.a); got != c.want {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d (симметрия)", c.b, c.a, got, c.want)
		}
	}
}

func TestSuggestKnownNamesThreshold(t *testing.T) {
	if got := suggestKnownNames("GOTCHA_HSTS_ENABLEDXX"); len(got) != 1 || got[0] != "GOTCHA_HSTS_ENABLED" {
		t.Errorf("suggestKnownNames(distance 2) = %v, want [GOTCHA_HSTS_ENABLED]", got)
	}
	if got := suggestKnownNames("GOTCHA_HSTS_ENABLEDXXX"); len(got) != 0 {
		t.Errorf("suggestKnownNames(distance 3) = %v, want ни одного кандидата", got)
	}
}

func TestSuggestKnownNamesDeterministicOrderWithTies(t *testing.T) {
	got := suggestKnownNames("GOTCHA_SMTP_HRT")
	want := []string{"GOTCHA_SMTP_HOST", "GOTCHA_SMTP_PORT"}
	if len(got) != len(want) {
		t.Fatalf("suggestKnownNames(\"GOTCHA_SMTP_HRT\") = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suggestKnownNames(\"GOTCHA_SMTP_HRT\")[%d] = %q, want %q (порядок %v)", i, got[i], want[i], got)
		}
	}
}

func TestSuggestKnownNamesNoCandidate(t *testing.T) {
	if got := suggestKnownNames("GOTCHA_TOTALLY_UNKNOWN_NAME_THAT_MATCHES_NOTHING"); len(got) != 0 {
		t.Errorf("suggestKnownNames = %v, want пустой список", got)
	}
}

// Ближайший кандидат обязан идти первым независимо от алфавита (HOST < PORT
// по буквам, но PORT ближе к GOTCHA_SMTP_PONT).
func TestSuggestKnownNamesOrdersByDistanceFirst(t *testing.T) {
	got := suggestKnownNames("GOTCHA_SMTP_PONT")
	want := []string{"GOTCHA_SMTP_PORT", "GOTCHA_SMTP_HOST"}
	if len(got) != len(want) {
		t.Fatalf("suggestKnownNames(\"GOTCHA_SMTP_PONT\") = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suggestKnownNames(\"GOTCHA_SMTP_PONT\")[%d] = %q, want %q (ближайший по расстоянию первым, а не по алфавиту)", i, got[i], want[i])
		}
	}
}
