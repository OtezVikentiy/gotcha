package guards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Restart     string            `yaml:"restart"`
	MemLimit    string            `yaml:"mem_limit"`
	Ports       []string          `yaml:"ports"`
	Environment map[string]string `yaml:"environment"`
	Logging     struct {
		Options map[string]string `yaml:"options"`
	} `yaml:"logging"`
	Healthcheck map[string]any `yaml:"healthcheck"`
	SecurityOpt []string       `yaml:"security_opt"`
	CapDrop     []string       `yaml:"cap_drop"`
	ReadOnly    bool           `yaml:"read_only"`
	PidsLimit   int            `yaml:"pids_limit"`
}

func loadCompose(t *testing.T, root, name string) composeFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("разбор %s: %v", name, err)
	}
	return cf
}

// Compose читает .env дважды — для подстановки ${…} и через env_file — и
// раскомментированное значение в .env побеждает дефолт compose.
func TestEnvExampleDoesNotOverrideCompose(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	cf := loadCompose(t, root, "docker-compose.yml")
	gotcha, ok := cf.Services["gotcha"]
	if !ok {
		t.Fatal("в docker-compose.yml нет сервиса gotcha — сторож ослеп")
	}
	keys := []string{"GOTCHA_LISTEN_ADDR"}
	for k := range gotcha.Environment {
		keys = append(keys, k)
	}
	if len(keys) < 4 {
		t.Fatalf("найдено %d переменных environment, ожидалось ≥4 — обход ослеп", len(keys))
	}
	env, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatalf(".env.example: %v", err)
	}
	lines := strings.Split(string(env), "\n")
	for _, k := range keys {
		for n, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), k+"=") {
				t.Errorf(".env.example:%d: %s раскомментирована, а compose задаёт своё значение — "+
					"скопированный .env молча перекроет его (находка №37)", n+1, k)
			}
		}
	}
}

func TestComposeServicesAreBounded(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	cf := loadCompose(t, root, "docker-compose.yml")
	if len(cf.Services) < 3 {
		t.Fatalf("в docker-compose.yml %d сервисов, ожидалось ≥3 — обход ослеп", len(cf.Services))
	}
	for name, svc := range cf.Services {
		if svc.Restart == "" {
			t.Errorf("docker-compose.yml: сервис %s без restart — после перезагрузки хоста "+
				"он останется лежать (добавили сервис и не дали ему политику)", name)
		}
		for _, opt := range []string{"max-size", "max-file"} {
			if svc.Logging.Options[opt] == "" {
				t.Errorf("docker-compose.yml: сервис %s без logging.options.%s — json-file "+
					"растёт без границы и забивает диск хоста (добавили сервис и не дали ему потолок)", name, opt)
			}
		}
		if svc.MemLimit == "" {
			t.Errorf("docker-compose.yml: сервис %s без mem_limit — OOM-killer хоста сам "+
				"выберет жертву (находка №39: добавили сервис и не дали ему потолок)", name)
		}
		if svc.Healthcheck == nil {
			t.Errorf("docker-compose.yml: сервис %s без healthcheck — зависший процесс "+
				"числится Up бесконечно", name)
		} else if _, ok := svc.Healthcheck["start_period"]; !ok {
			t.Errorf("docker-compose.yml: healthcheck сервиса %s без start_period — бюджет "+
				"проверок уйдёт на инициализацию (находка №104)", name)
		}
	}

	// Ужесточение приложения: у gotcha нет причин иметь capabilities, запись
	// в свою ФС или неограниченное число процессов.
	gotcha := cf.Services["gotcha"]
	// Дефолт обязан быть true: без него или с false ужесточение гаснет молча у всех.
	// Подстановка нужна хостам с AppArmor-confined dockerd (snap) — там иначе падает exec.
	const (
		nnpLiteral = "no-new-privileges:true"
		nnpSubst   = "no-new-privileges:${GOTCHA_COMPOSE_NO_NEW_PRIVS:-true}"
	)
	// Сверяется весь список, не только наличие нужной записи — иначе дописанное
	// ослабление (apparmor=unconfined, seccomp=unconfined) прошло бы молча.
	if len(gotcha.SecurityOpt) != 1 ||
		(gotcha.SecurityOpt[0] != nnpLiteral && gotcha.SecurityOpt[0] != nnpSubst) {
		t.Errorf("docker-compose.yml: security_opt сервиса gotcha = %q, а обязан состоять "+
			"ровно из одной записи — %q или %q (дефолт поставки включает no-new-privileges, "+
			"а посторонних опций в списке быть не должно)",
			gotcha.SecurityOpt, nnpLiteral, nnpSubst)
	}
	hasAll := false
	for _, c := range gotcha.CapDrop {
		if c == "ALL" {
			hasAll = true
		}
	}
	if !hasAll {
		t.Error("docker-compose.yml: gotcha без cap_drop: [ALL]")
	}
	if !gotcha.ReadOnly {
		t.Error("docker-compose.yml: gotcha без read_only: true")
	}
	if gotcha.PidsLimit <= 0 {
		t.Error("docker-compose.yml: gotcha без pids_limit")
	}

	// Требований к содержимому нет — только чтобы YAML читался.
	loadCompose(t, root, "docker-compose.small.yml")
}

// Порт обязан по умолчанию биндиться на loopback: голый HTTP-вход и
// открытый /metrics не должны быть доступны в обход прокси с любого адреса.
func TestComposeGotchaPortBindsLoopbackByDefault(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	cf := loadCompose(t, root, "docker-compose.yml")
	gotcha, ok := cf.Services["gotcha"]
	if !ok {
		t.Fatal("в docker-compose.yml нет сервиса gotcha — сторож ослеп")
	}
	if len(gotcha.Ports) == 0 {
		t.Fatal("сервис gotcha не публикует ни одного порта — сторож ослеп")
	}
	for _, p := range gotcha.Ports {
		if !strings.Contains(p, "${GOTCHA_COMPOSE_BIND:-127.0.0.1}") {
			t.Errorf("docker-compose.yml: порт %q публикуется без дефолтного бинда на loopback "+
				"(${GOTCHA_COMPOSE_BIND:-127.0.0.1}:...) — риск снова публиковать приложение на 0.0.0.0 по умолчанию", p)
		}
	}
}

var composeSubstRe = regexp.MustCompile(`\$\{(GOTCHA_[A-Z0-9_]+)(?::[-?][^}]*)?\}`)

// Исключение — только когда ключ YAML совпадает с именем переменной, и она
// реально читается cmd/gotcha/config.go (configVars), а не любое такое совпадение.
func TestComposeVarsNamespaced(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	configVars := collectGotchaEnvVars(t, root, filepath.Join("cmd", "gotcha", "config.go"), nil)
	if len(configVars) < 20 {
		t.Fatalf("обход ослеп: cmd/gotcha/config.go даёт только %d переменных, ожидалось ≥20", len(configVars))
	}
	total := 0
	for _, name := range []string{"docker-compose.yml", "docker-compose.small.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("разбор %s: %v", name, err)
		}
		total += checkComposeNamespace(t, name, "", &doc, configVars)
	}
	if total < 10 {
		t.Fatalf("найдено %d подстановок GOTCHA_* по всем compose-файлам, ожидалось ≥10 — обход ослеп", total)
	}
}

func checkComposeNamespace(t *testing.T, file, parentKey string, n *yaml.Node, configVars map[string]bool) int {
	t.Helper()
	found := 0
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			found += checkComposeNamespace(t, file, "", c, configVars)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			found += checkComposeNamespace(t, file, n.Content[i].Value, n.Content[i+1], configVars)
		}
	case yaml.ScalarNode:
		for _, m := range composeSubstRe.FindAllStringSubmatch(n.Value, -1) {
			found++
			varName := m[1]
			if varName == parentKey && configVars[varName] {
				continue // проброс: ключ окружения совпадает с именем переменной, И это реально поле cmd/gotcha.Config
			}
			if !strings.HasPrefix(varName, "GOTCHA_COMPOSE_") && !strings.HasPrefix(varName, "GOTCHA_BUILD_") {
				t.Errorf("%s:%d: подстановка ${%s} без префикса GOTCHA_COMPOSE_/GOTCHA_BUILD_ — "+
					"эту переменную читает только сам Docker Compose, cmd/gotcha.Config поля под неё нет",
					file, n.Line, varName)
			}
		}
	}
	return found
}

// У compose-only переменных читатель — не Go-код, а сама подстановка в
// docker-compose.yml, поэтому сторожи, берущие имена из кода, их не видят.
func TestComposeVarsDocumented(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	composeVars := map[string]bool{}
	for _, name := range []string{"docker-compose.yml", "docker-compose.small.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range composeSubstRe.FindAllStringSubmatch(string(raw), -1) {
			v := m[1]
			if strings.HasPrefix(v, "GOTCHA_COMPOSE_") || strings.HasPrefix(v, "GOTCHA_BUILD_") {
				composeVars[v] = true
			}
		}
	}
	if len(composeVars) < 8 {
		t.Fatalf("найдено %d переменных GOTCHA_COMPOSE_*/GOTCHA_BUILD_* по всем compose-файлам, ожидалось ≥8 — обход ослеп", len(composeVars))
	}

	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	ruDoc, err := os.ReadFile(filepath.Join(root, "internal", "docs", "ru", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	enDoc, err := os.ReadFile(filepath.Join(root, "internal", "docs", "en", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	ruTable := tableVarNames(string(ruDoc))
	enTable := tableVarNames(string(enDoc))

	for v := range composeVars {
		if !strings.Contains(string(example), v+"=") {
			t.Errorf("%s подставляется Docker Compose (${%s}), но отсутствует в .env.example", v, v)
		}
		if !ruTable[v] {
			t.Errorf("ru: %s подставляется Docker Compose, но не задокументирована строкой таблицы в internal/docs/ru/configuration.md", v)
		}
		if !enTable[v] {
			t.Errorf("en: %s подставляется Docker Compose, но не задокументирована строкой таблицы в internal/docs/en/configuration.md", v)
		}
	}
}
