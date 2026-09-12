package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var agentEnvNameRe = regexp.MustCompile(`GOTCHA_AGENT_[A-Z_]+`)

var mustAppearInHostsGo = []string{
	"GOTCHA_AGENT_ENDPOINT",
	"GOTCHA_AGENT_INGEST_KEY",
}

var hostsGoExclusions = map[string]string{
	"GOTCHA_AGENT_HOSTNAME":                 "override host.name; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_CA_CERT":                  "путь к CA для самоподписанного инстанса; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_INTERVAL_SECONDS":         "интервал сбора; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": "крайнее средство вместо CA_CERT; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_ENVIRONMENT":              "resource-метка deployment.environment; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_ROLE":                     "resource-метка host.role; необязательная настройка агента, не часть auth-only команды UI",
}

var mustAppearInInstallSh = []string{
	"GOTCHA_AGENT_ENDPOINT",
	"GOTCHA_AGENT_INGEST_KEY",
	"GOTCHA_AGENT_INTERVAL_SECONDS",
	"GOTCHA_AGENT_HOSTNAME",
	"GOTCHA_AGENT_CA_CERT",
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY",
	"GOTCHA_AGENT_ENVIRONMENT",
	"GOTCHA_AGENT_ROLE",
}

// Пуст сегодня — оставлен как map: TestAgentEnvVarsClassified требует классификации
// каждой новой переменной сюда или в mustAppearInInstallSh.
var installShExclusions = map[string]string{}

func agentConfigEnvVarSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	vars := collectGotchaEnvVars(t, root, filepath.Join("internal", "agent", "config.go"), nil)
	if len(vars) < 8 {
		t.Fatalf("обход ослеп: канонических переменных агента найдено %d, ожидалось не меньше 8", len(vars))
	}
	return vars
}

func containsName(list []string, name string) bool {
	for _, x := range list {
		if x == name {
			return true
		}
	}
	return false
}

// Гарантирует ФАКТ классификации (причина непуста), не её ПРАВИЛЬНОСТЬ — переменную,
// отнесённую не в тот список по ошибке, этот тест не поймает.
func TestAgentEnvVarsClassified(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	for name := range canon {
		inMust := containsName(mustAppearInHostsGo, name)
		reason, inExcl := hostsGoExclusions[name]
		switch {
		case inMust && inExcl:
			t.Errorf("%s одновременно в mustAppearInHostsGo и в hostsGoExclusions — противоречие, убрать из одного из двух", name)
		case !inMust && !inExcl:
			t.Errorf("%s — новая каноническая переменная internal/agent/config.go, не классифицированная для hosts.go: "+
				"добавить в mustAppearInHostsGo (если должна быть в команде установки) или в hostsGoExclusions с причиной (если не должна)", name)
		case inExcl && strings.TrimSpace(reason) == "":
			t.Errorf("hostsGoExclusions[%s] — пустая причина: исключение обязано объяснять, почему переменная не в mustAppearInHostsGo", name)
		}
	}
	for _, name := range mustAppearInHostsGo {
		if !canon[name] {
			t.Errorf("mustAppearInHostsGo содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
	for name := range hostsGoExclusions {
		if !canon[name] {
			t.Errorf("hostsGoExclusions содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}

	for name := range canon {
		inMust := containsName(mustAppearInInstallSh, name)
		reason, inExcl := installShExclusions[name]
		switch {
		case inMust && inExcl:
			t.Errorf("%s одновременно в mustAppearInInstallSh и в installShExclusions — противоречие, убрать из одного из двух", name)
		case !inMust && !inExcl:
			t.Errorf("%s — новая каноническая переменная internal/agent/config.go, не классифицированная для install.sh: "+
				"добавить в mustAppearInInstallSh (если должна быть в скрипте) или в installShExclusions с причиной (если не должна)", name)
		case inExcl && strings.TrimSpace(reason) == "":
			t.Errorf("installShExclusions[%s] — пустая причина: исключение обязано объяснять, почему install.sh её не пишет", name)
		}
	}
	for _, name := range mustAppearInInstallSh {
		if !canon[name] {
			t.Errorf("mustAppearInInstallSh содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
	for name := range installShExclusions {
		if !canon[name] {
			t.Errorf("installShExclusions содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
}

// Строковые литералы через go/ast: комментарии в AST не попадают, и "//" внутри raw-строки
// (https://) не путается с началом комментария, как было бы у построчного regex-стриппера.
func hostsGoAgentNames(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "web", "hosts.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse hosts.go: %v", err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		names = append(names, agentEnvNameRe.FindAllString(lit.Value, -1)...)
		return true
	})
	return names
}

// Полнострочный "#" — install.sh сегодня не несёт инлайн-# после кода (сверено вручную).
func stripShellLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func TestHostsGoInstallCommandVarsMatchAgentConfig(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	found := hostsGoAgentNames(t, root)
	if len(found) < 2 {
		t.Fatalf("обход ослеп: в строковых литералах internal/web/hosts.go найдено %d упоминаний GOTCHA_AGENT_*, ожидалось не меньше 2", len(found))
	}
	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !canon[name] {
			t.Errorf("internal/web/hosts.go ссылается на %s, которой нет среди переменных internal/agent/config.go — "+
				"команду установки переименовали в одном месте и забыли другое", name)
		}
	}

	for _, name := range mustAppearInHostsGo {
		if !canon[name] {
			t.Fatalf("обход ослеп: mustAppearInHostsGo содержит %s, которой нет в каноне internal/agent/config.go — сам список устарел", name)
		}
		if !seen[name] {
			t.Errorf("internal/web/hosts.go больше не упоминает %s — переменную добавили/переименовали в internal/agent/config.go, "+
				"а команду установки не поправили следом", name)
		}
	}
}

func TestInstallShVarsMatchAgentConfig(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	raw, err := os.ReadFile(filepath.Join(root, "internal", "web", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	code := stripShellLineComments(string(raw))

	found := agentEnvNameRe.FindAllString(code, -1)
	if len(found) < 6 {
		t.Fatalf("обход ослеп: в коде install.sh (без комментариев) найдено %d упоминаний GOTCHA_AGENT_*, ожидалось не меньше 6", len(found))
	}
	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !canon[name] {
			t.Errorf("install.sh ссылается на %s, которой нет среди переменных internal/agent/config.go — "+
				"переименовали переменную в config.go и забыли install.sh", name)
		}
	}

	for _, name := range mustAppearInInstallSh {
		if !canon[name] {
			t.Fatalf("обход ослеп: mustAppearInInstallSh содержит %s, которой нет в каноне internal/agent/config.go — сам список устарел", name)
		}
		if !seen[name] {
			t.Errorf("install.sh больше не упоминает %s — переменную добавили/переименовали в internal/agent/config.go, "+
				"а install.sh не поправили следом", name)
		}
	}
}

// install.sh обязан прогнать --check НОВЫМ бинарём ДО подмены боевого — иначе на устаревшем $CONF
// systemd поймает exit 2 (RestartPreventExitStatus=2) и не откатится на рабочий старый бинарь.
func TestInstallShChecksBeforeSwappingBinary(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "internal", "web", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	code := stripShellLineComments(string(raw))

	checkIdx := strings.Index(code, `"$BIN.new" --check`)
	mvIdx := strings.Index(code, `mv "$BIN.new" "$BIN"`)
	if checkIdx < 0 {
		t.Fatal(`обход ослеп: install.sh не содержит вызов "$BIN.new" --check`)
	}
	if mvIdx < 0 {
		t.Fatal(`обход ослеп: install.sh не содержит mv "$BIN.new" "$BIN"`)
	}
	if mvIdx < checkIdx {
		t.Errorf(`install.sh подменяет боевой бинарь (mv "$BIN.new" "$BIN") РАНЬШЕ, чем проверяет конфиг ("$BIN.new" --check) — на устаревших именах в $CONF это гасит юнит на первом же рестарте вместо того, чтобы оставить работать старый бинарь`)
	}
}
