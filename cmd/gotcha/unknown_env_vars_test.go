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

// TestCheckUnknownEnvVarsAcceptsKnownServerVars — переменные, реально
// читаемые cmd/gotcha, не отказывают старту (регрессия: сама проверка не
// должна отказывать на легитимном имени, ради которого её и завели).
func TestCheckUnknownEnvVarsAcceptsKnownServerVars(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom(
		"GOTCHA_BASE_URL=https://gotcha.example.com",
		"GOTCHA_HSTS_ENABLED=true",
		"GOTCHA_EVALUATORS_ENABLED=false",
	)); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для известных серверных имён", err)
	}
}

// TestCheckUnknownEnvVarsAcceptsAgentVarsOnServer — прод-сценарий из брифа:
// агент штатно ставится на тот же хост, что и сервер, с общим `.env`.
// GOTCHA_AGENT_INGEST_KEY в окружении процесса gotcha (не gotcha-agent) —
// легитимный сосед по файлу, а не опечатка.
func TestCheckUnknownEnvVarsAcceptsAgentVarsOnServer(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("GOTCHA_AGENT_INGEST_KEY=some-ingest-key")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для GOTCHA_AGENT_INGEST_KEY в окружении сервера", err)
	}
}

// TestCheckUnknownEnvVarsAcceptsComposeAndBuildPrefixes — второй
// прод-сценарий из брифа: `docker-compose.yml` подключает `env_file: .env`
// целиком, так что compose-only и build-only переменные легитимно попадают
// в окружение процесса gotcha, который их не читает и знать поимённо не
// обязан — исключены целиком по префиксу.
func TestCheckUnknownEnvVarsAcceptsComposeAndBuildPrefixes(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom(
		"GOTCHA_COMPOSE_PG_PASSWORD=secret",
		"GOTCHA_BUILD_VERSION=1.2.3",
	)); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для GOTCHA_COMPOSE_*/GOTCHA_BUILD_*", err)
	}
}

// TestCheckUnknownEnvVarsIgnoresNonGotchaVars — переменные без префикса
// GOTCHA_ (PATH и подобные, штатно присутствующие в окружении любого
// процесса) проверку не касаются вовсе.
func TestCheckUnknownEnvVarsIgnoresNonGotchaVars(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("PATH=/usr/bin", "HOME=/root")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для переменных без префикса GOTCHA_", err)
	}
}

// TestCheckUnknownEnvVarsIsCasePreserving — префикс "GOTCHA_" сравнивается
// РЕГИСТРОЗАВИСИМО: "gotcha_lower" и "Gotcha_Port" не наши переменные вовсе
// (продукт всегда пишет имена капсом, см. конвенцию именования), а не
// "неизвестные GOTCHA_*" — strings.HasPrefix(name, "GOTCHA_") обязан
// остаться без нормализации регистра перед сравнением. Без этого теста
// вставка strings.ToUpper(name) перед проверкой префикса (или аналогичная
// мутация) не ловится ни одним другим тестом.
func TestCheckUnknownEnvVarsIsCasePreserving(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("gotcha_lower=1", "Gotcha_Port=1")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil — 'gotcha_lower'/'Gotcha_Port' не начинаются с 'GOTCHA_' регистрозависимо, это не наши переменные", err)
	}
}

// TestCheckUnknownEnvVarsIgnoresMalformedEntries — запись без "=" (которую
// os.Environ() в реальности не производит никогда) не паникует и не
// считается неизвестным именем: strings.Cut возвращает ok=false, вся
// строка целиком пропускается тем же условием, что отсекает переменные без
// префикса GOTCHA_.
func TestCheckUnknownEnvVarsIgnoresMalformedEntries(t *testing.T) {
	if err := checkUnknownEnvVars(environFrom("GOTCHA_WEIRD_NO_EQUALS_SIGN")); err != nil {
		t.Errorf("checkUnknownEnvVars: %v, want nil для записи без '='", err)
	}
}

// TestCheckUnknownEnvVarsRejectsTypo — B12/бриф: GOTCHA_HSTS_ENABLE (без
// D) сегодня молча проходит с дефолтом; после этой задачи — отказ старта с
// подсказкой ближайшего известного имени.
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

// TestCheckUnknownEnvVarsListsAllFindingsSorted — несколько неизвестных
// имён сразу: сообщение перечисляет все, в детерминированном (алфавитном)
// порядке — не в порядке os.Environ(), который ничего не гарантирует.
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

// sortedAgentOwnedOldNames — envcontract.AgentOwned, отсортировано, для
// детерминированного выбора одного из трёх старых АГЕНТСКИХ имён.
// Динамически, не литералом: internal/guards/renamed_env_vars_test.go
// (TestNoRenamedEnvVarNames) не пускает старые имена литералом за пределы
// renamed.go/CHANGELOG/upgrade.md/renamed_env_contract_test.go.
func sortedAgentOwnedOldNames() []string {
	names := make([]string, len(envcontract.AgentOwned))
	copy(names, envcontract.AgentOwned)
	sort.Strings(names)
	return names
}

// TestCheckUnknownEnvVarsRejectsEmptyRenamedName — W3-1 (повторное ревью):
// одна и та же переменная общего .env хоста обязана давать один и тот же
// вердикт у сервера и у агента. Живой прогон нашёл: агентское старое имя
// (envcontract.AgentOwned) с ПУСТЫМ значением на сервере говорило «unknown,
// check for typos», а на агенте — «renamed to» (см.
// internal/agent/config.go). Причина: CheckRenamedAll (loadConfig, самая
// первая операция) проверяет только НЕПУСТОЕ значение — declared-but-unset
// для по-настоящему живого имени легитимно, — так что переименованное имя
// с пустым значением долетало сюда, минуя её, и checkUnknownEnvVars до этой
// правки не знала про envcontract.Renamed вовсе. Тот же контракт, что у
// internal/agent.checkUnknownAgentEnvVars: declared-but-unset не спасает
// имя, которое не читает уже никто.
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

// TestLoadConfigCheckedOrderRenamedBeforeUnknown — устаревшее имя из
// envcontract.Renamed обязано получить ТОЧНЫЙ ответ CheckRenamedAll
// ("renamed to NEW_NAME"), а не догадку checkUnknownEnvVars по Левенштейну:
// старое имя не входит в envcontract.Known (оно больше не читается), так
// что без правильного порядка вызовов в loadConfigChecked оно попало бы под
// checkUnknownEnvVars как обычная опечатка. Имя берётся из
// envcontract.Renamed динамически (sortedRenamedOldNames), а не литералом —
// internal/guards/renamed_env_vars_test.go (TestNoRenamedEnvVarNames) не
// пускает старые имена литералом за пределы renamed_env_contract_test.go.
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

// TestLoadConfigCheckedPropagatesUnknownAfterRenamedPasses — когда
// устаревших имён нет, но есть опечатка в текущем, loadConfigChecked
// доходит до checkUnknownEnvVars и возвращает её ошибку.
func TestLoadConfigCheckedPropagatesUnknownAfterRenamedPasses(t *testing.T) {
	_, err := loadConfigChecked(getenvFrom(nil), environFrom("GOTCHA_HSTS_ENABLE=false"), nil)
	if err == nil {
		t.Fatal("loadConfigChecked: want ошибку на GOTCHA_HSTS_ENABLE, получили nil")
	}
	if !strings.Contains(err.Error(), "GOTCHA_HSTS_ENABLE") {
		t.Errorf("err = %q, want упоминание GOTCHA_HSTS_ENABLE", err)
	}
}

// TestLoadConfigCheckedSucceedsOnCleanEnv — путь без единой находки:
// loadConfigChecked отдаёт тот же Config, что и loadConfig, err == nil.
func TestLoadConfigCheckedSucceedsOnCleanEnv(t *testing.T) {
	cfg, err := loadConfigChecked(getenvFrom(nil), environFrom(), nil)
	if err != nil {
		t.Fatalf("loadConfigChecked: %v, want nil на чистом окружении", err)
	}
	if cfg.Mode != "all" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "all")
	}
}

// levenshteinDistanceCases — таблица, ПО КОТОРОЙ ИТЕРИРУЮТ (не фикстура,
// защищающая одну строку из многих): классические регрессии алгоритма
// редактирования плюс характерные для проекта опечатки имён переменных.
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
		// Симметрично: расстояние редактирования не зависит от порядка
		// аргументов.
		if got := levenshteinDistance(c.b, c.a); got != c.want {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d (симметрия)", c.b, c.a, got, c.want)
		}
	}
}

// TestSuggestKnownNamesThreshold — граница порога maxSuggestDistance (2):
// расстояние 2 до GOTCHA_HSTS_ENABLED предлагается, расстояние 3 до того
// же имени — уже нет. Оба случая построены от одного известного имени,
// чтобы разница была ровно в пороге, а не в случайно подвернувшемся имени.
func TestSuggestKnownNamesThreshold(t *testing.T) {
	if got := suggestKnownNames("GOTCHA_HSTS_ENABLEDXX"); len(got) != 1 || got[0] != "GOTCHA_HSTS_ENABLED" {
		t.Errorf("suggestKnownNames(distance 2) = %v, want [GOTCHA_HSTS_ENABLED]", got)
	}
	if got := suggestKnownNames("GOTCHA_HSTS_ENABLEDXXX"); len(got) != 0 {
		t.Errorf("suggestKnownNames(distance 3) = %v, want ни одного кандидата", got)
	}
}

// TestSuggestKnownNamesDeterministicOrderWithTies — несколько кандидатов на
// одном расстоянии: GOTCHA_SMTP_HRT в двух правках от GOTCHA_SMTP_HOST и в
// двух от GOTCHA_SMTP_PORT — порядок обязан быть алфавитным, а не порядком
// обхода map (envcontract.Known), который недетерминирован сам по себе.
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

// TestSuggestKnownNamesNoCandidate — имя, не похожее ни на одно известное
// в пределах порога: пустой список кандидатов, а не паника или произвольный
// результат.
func TestSuggestKnownNamesNoCandidate(t *testing.T) {
	if got := suggestKnownNames("GOTCHA_TOTALLY_UNKNOWN_NAME_THAT_MATCHES_NOTHING"); len(got) != 0 {
		t.Errorf("suggestKnownNames = %v, want пустой список", got)
	}
}

// TestSuggestKnownNamesOrdersByDistanceFirst — находка ревью: ветка
// sort.Slice, где кандидаты РАЗЛИЧАЮТСЯ расстоянием (candidates[i].dist !=
// candidates[j].dist), раньше не была покрыта ни одним тестом —
// TestSuggestKnownNamesDeterministicOrderWithTies проверяет только ничью
// (оба расстояния равны). GOTCHA_SMTP_PONT — расстояние 1 до
// GOTCHA_SMTP_PORT и расстояние 2 до GOTCHA_SMTP_HOST: ближайший кандидат
// обязан идти первым независимо от алфавита (HOST < PORT по буквам, но
// PORT ближе и обязан быть первым).
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
