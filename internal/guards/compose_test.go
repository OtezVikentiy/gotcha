package guards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeFile/composeService — ровно те поля compose-файлов, на которые
// опираются правила. Разбор строгий не нужен: неизвестные поля игнорируются.
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

// TestEnvExampleDoesNotOverrideCompose — каждая переменная, которую задаёт
// секция environment сервиса gotcha, обязана быть закомментирована в
// .env.example. Compose читает .env дважды — для подстановки ${…} и через
// env_file — поэтому раскомментированное значение в .env ПОБЕЖДАЕТ дефолт
// compose: так появилась находка №37 (BASE_URL с портом 8080 против
// публикуемого 59080 → 403 на каждый POST). GOTCHA_LISTEN_ADDR добавлен явно:
// compose его не задаёт, но проброс порта 59080:8080 подразумевает :8080.
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
	// Нижняя граница: environment сервиса задаёт минимум три переменные
	// (PG_DSN, CH_DSN, BASE_URL, SECRET_KEY на момент написания). Меньше —
	// сторож разучился читать YAML, а не compose стал проще.
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

// TestComposeServicesAreBounded — каждый сервис базового compose-файла обязан
// иметь restart-политику, ротацию логов, потолок памяти и healthcheck со
// start_period. Класс дефекта — «добавили сервис и не дали ему потолок»:
// ровно так появились находки №39 (память ClickHouse без лимита ела 90%
// хоста) и №104 (без start_period бюджет проверок тратился на инициализацию,
// и первый запуск на минимальном VPS падал «dependency failed to start»).
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
	hasNNP := false
	for _, o := range gotcha.SecurityOpt {
		if o == "no-new-privileges:true" {
			hasNNP = true
		}
	}
	if !hasNNP {
		t.Error("docker-compose.yml: gotcha без security_opt no-new-privileges:true")
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

	// Оверлей для стеснённых машин: требований к перекрытиям нет, но
	// нечитаемый YAML — ошибка здесь, раньше CI (тот проверяет связку целиком).
	loadCompose(t, root, "docker-compose.small.yml")
}

// TestComposeGotchaPortBindsLoopbackByDefault — публикация порта приложения
// (ports: у сервиса gotcha) обязана по умолчанию биндиться ТОЛЬКО на
// loopback (${GOTCHA_COMPOSE_BIND:-127.0.0.1}), а не на все интерфейсы (W3-D,
// запись 6, ревью 2026-08-27). Раньше compose публиковал 59080 на 0.0.0.0
// безусловно: чек-лист прод-развёртывания (installation.md) ставит перед
// приложением TLS-реверс-прокси, но голый HTTP-вход и открытый /metrics
// (self-телеметрия без аутентификации) оставались доступны напрямую В ОБХОД
// прокси с ЛЮБОГО адреса. YAML юнит-тестами не проверяется — этот сторож
// ловит будущую правку compose, которая молча вернёт бинд на 0.0.0.0.
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

// composeSubstRe находит подстановки Docker Compose ${GOTCHA_...} (с
// дефолтом `:-...`/`:?...` или без него) в тексте compose-файла.
var composeSubstRe = regexp.MustCompile(`\$\{(GOTCHA_[A-Z0-9_]+)(?::[-?][^}]*)?\}`)

// TestComposeVarsNamespaced — любая переменная GOTCHA_*, которую подставляет
// сам Docker Compose (`${GOTCHA_...}` в docker-compose.yml/.small.yml),
// обязана нести префикс GOTCHA_COMPOSE_ или GOTCHA_BUILD_ (envcontract.Renamed,
// «E3, заморозка контракта — неймспейс compose и сборки»). Без него имя
// неотличимо на вид от обычной продуктовой переменной, а cmd/gotcha.Config
// поля под него нет и не будет: оператор, задавший, скажем, GOTCHA_PG_PASSWORD
// напрямую на Kubernetes/systemd (без Compose в цепочке), не получает ни
// эффекта, ни диагностики — сам compose её просто не подставит.
//
// Одно сквозное исключение, найденное структурно, а не вторым списком
// руками: подстановка, которую compose пробрасывает под ТЕМ ЖЕ именем в
// окружение контейнера (`GOTCHA_BASE_URL: ${GOTCHA_BASE_URL:-...}`,
// `GOTCHA_SECRET_KEY: ${GOTCHA_SECRET_KEY:-...}`) — это не compose-only
// переменная, а обычное поле cmd/gotcha.Config, которое compose лишь
// форвардит дальше под собственным именем. Любая другая подстановка —
// под другим ключом (`POSTGRES_PASSWORD: ${GOTCHA_PG_PASSWORD:-gotcha}`,
// `VERSION: ${GOTCHA_VERSION:-dev}`) или вовсе без ключа (`ports:`,
// `mem_limit:`, `driver_opts:`) — исключения не получает и обязана нести
// префикс.
//
// Критерий исключения — не только «ключ YAML совпадает с именем переменной»
// (находка M2 ревью): такое совпадение само по себе ничего не
// доказывает — `GOTCHA_NET_MTU: ${GOTCHA_NET_MTU:-1450}` в environment прошло
// бы этот критерий, хотя GOTCHA_NET_MTU не поле cmd/gotcha.Config, а
// переименованная (в этой же волне) compose-only переменная. Исключение
// действует, только если имя ЕЩЁ И реально читает cmd/gotcha/config.go —
// сверяется с configVars (collectGotchaEnvVars, env_example_test.go, та же
// истина, которой поверяет .env.example) — а не со вторым списком имён
// вручную.
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

// checkComposeNamespace обходит YAML-дерево одного compose-файла и проверяет
// каждую найденную подстановку GOTCHA_* (см. докблок TestComposeVarsNamespaced
// про сквозное исключение "ключ совпадает с именем переменной, которое
// реально читает cmd/gotcha.Config"). Возвращает число найденных подстановок
// — по нему TestComposeVarsNamespaced проверяет, что обход не ослеп.
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

// TestComposeVarsDocumented — задача 11, пункт 4: паритет compose-неймспейса
// (${GOTCHA_COMPOSE_*}/${GOTCHA_BUILD_*}, подстановка Docker Compose самого
// себя — читатель) ↔ .env.example ↔ таблицы configuration.md обеих локалей.
// Ruling задачи 11 п.5: у этих имён читатель — не Go-код, а сама подстановка
// в docker-compose.yml/.small.yml, поэтому TestEnvExampleCoversConfig
// (которая сверяет .env.example с кодом) и checkConfigurationTableParity
// (которая берёт vars из кода) их не видят вовсе — этот сторож закрывает
// именно этот, третий класс переменных, отдельно.
//
// До круга правок задачи 11 GOTCHA_COMPOSE_PORT, GOTCHA_COMPOSE_BIND и все
// три GOTCHA_BUILD_* вовсе отсутствовали в .env.example — реальная находка,
// которую этот сторож теперь не даст повторить.
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
		// То же «NAME=», что и в TestEnvExampleCoversConfig — короткое имя
		// префикс более длинного (GOTCHA_COMPOSE_PG_PASSWORD ⊂
		// GOTCHA_COMPOSE_CH_PASSWORD не бывает, но конвенция общая с тем
		// сторожем не случайно).
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
