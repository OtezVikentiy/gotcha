package guards

import (
	"os"
	"path/filepath"
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
// публикуемого 59080 → 403 на каждый POST). GOTCHA_ADDR добавлен явно:
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
	keys := []string{"GOTCHA_ADDR"}
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
// loopback (${GOTCHA_BIND:-127.0.0.1}), а не на все интерфейсы (W3-D,
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
		if !strings.Contains(p, "${GOTCHA_BIND:-127.0.0.1}") {
			t.Errorf("docker-compose.yml: порт %q публикуется без дефолтного бинда на loopback "+
				"(${GOTCHA_BIND:-127.0.0.1}:...) — риск снова публиковать приложение на 0.0.0.0 по умолчанию", p)
		}
	}
}
