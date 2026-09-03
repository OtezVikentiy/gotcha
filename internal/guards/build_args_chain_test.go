package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
	"gopkg.in/yaml.v3"
)

// TestBuildArgsChainWired — цепочка проброса версии в образ (задача 9, круг
// правок 1, находка M5) не была защищена НИ ОДНИМ тестом: переименование
// `ARG VERSION` → `ARG APP_VERSION` в Dockerfile (без единого литерала из
// envcontract.Renamed) оставляло build/vet/gofmt/весь остальной набор
// сторожей зелёными, а `/healthz` в проде тихо показывал бы «dev» вместо
// версии релиза. `docker compose config`, которым это пытался закрыть
// первый круг, останавливается на YAML-подстановке и до Dockerfile/ldflags
// не доходит вовсе.
//
// Проверка статическая, реального `make build`/docker нет (докер — дело
// гейта, не юнит-теста): цепочка разобрана СТАТИЧЕСКИ по звеньям, каждое —
// свой ассерт с именем звена, именем переменной и файлом, чтобы падение
// сразу указывало, что именно разъехалось:
//
//  1. Makefile: DOCKER_BUILD_ENV выставляет РОВНО набор GOTCHA_BUILD_*
//     имён — сверяется с истиной, envcontract.InfraOwned/Renamed (та же
//     таблица, что renamed_env_vars_test.go), а не со вторым списком
//     руками.
//  2. docker-compose.yml: у сервиса gotcha в build.args для каждого
//     GOTCHA_BUILD_X есть ключ, чьё значение подставляет ${GOTCHA_BUILD_X}.
//     Пары «ключ arg → GOTCHA_BUILD_X» идут дальше в шаг 3.
//  3. Dockerfile: для каждого ключа arg из шага 2 есть `ARG <ключ>`.
//  4. Dockerfile: каждый ARG из шага 3 используется в -ldflags как
//     `-X <importpath>/internal/version.<sym>=${<ключ>}`. Пары «ключ → sym»
//     идут в шаг 5.
//  5. internal/version: каждый sym из шага 4 — реально существующая
//     переменная уровня пакета (go/ast, не грепом), и обработчик
//     `/healthz` (cmd/gotcha/health.go) реально читает версию через пакет
//     internal/version (импорт + вызов экспортированного идентификатора,
//     тоже по AST).
//
// Каждое звено ломается НЕЗАВИСИМО от остальных: переименовать ARG, убрать
// -X, переименовать переменную в internal/version, убрать имя из
// DOCKER_BUILD_ENV, убрать arg из compose — и падает ассерт именно этого
// звена, а не «что-то где-то не сошлось».
func TestBuildArgsChainWired(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	// Звено 1: истина — envcontract.InfraOwned/Renamed, не второй список.
	buildVars := buildEnvVarsFromContract(t)
	if len(buildVars) < 3 {
		t.Fatalf("обход ослеп: envcontract.InfraOwned содержит только %d имён GOTCHA_BUILD_*, ожидалось ≥3", len(buildVars))
	}

	makefileVars := makefileDockerBuildEnvVars(t, root)
	assertSameNameSet(t, "Makefile: DOCKER_BUILD_ENV", makefileVars, buildVars)

	// Звено 2: docker-compose.yml build.args сервиса gotcha.
	argKeyToBuildVar := composeBuildArgsForBuildVars(t, root, buildVars)

	// Звено 3: Dockerfile ARG <ключ> для каждого ключа arg из шага 2.
	dockerfileRaw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	dockerfile := string(dockerfileRaw)
	declaredArgs := dockerfileARGNames(dockerfile)
	for argKey, buildVar := range argKeyToBuildVar {
		if !declaredArgs[argKey] {
			t.Errorf("Dockerfile: build-arg %q (docker-compose.yml build.args подставляет ${%s}) "+
				"не объявлен через `ARG %s` — цепочка версии оборвана на Dockerfile", argKey, buildVar, argKey)
		}
	}

	// Звено 4: каждый ARG из шага 3 используется в -ldflags на символ пакета
	// internal/version — в RUN-блоке, который собирает СЕРВЕРНЫЙ бинарь
	// (/out/gotcha), а не в блоке кросс-сборки агента: /healthz — это
	// серверный процесс, и потеря -X именно в его RUN (а не в соседнем,
	// собирающем gotcha-agent) — ровно тот регресс, который проверка обязана
	// поймать. Общий на весь файл разбор пропустил бы такую потерю: те же
	// три -X встречаются ещё дважды в RUN агента, и глобальный поиск нашёл
	// бы совпадение там.
	serverBlock := dockerfileServerBuildBlock(t, dockerfile)
	argKeyToSym := dockerfileLdflagsSymbols(serverBlock)
	for argKey, buildVar := range argKeyToBuildVar {
		sym, ok := argKeyToSym[argKey]
		if !ok {
			t.Errorf("Dockerfile: ARG %s (← %s) объявлен, но ни разу не используется в -ldflags как "+
				"-X .../internal/version.<символ>=${%s} — сборка передаст версию в бинарь, только если "+
				"этот -X реально есть", argKey, buildVar, argKey)
			continue
		}

		// Звено 5а: sym — реальная переменная уровня пакета internal/version.
		if !versionPackageVarExists(t, root, sym) {
			t.Errorf("internal/version: -ldflags Dockerfile целится в internal/version.%s (ARG %s ← %s), "+
				"но такой переменной уровня пакета в internal/version/version.go нет", sym, argKey, buildVar)
		}
	}

	// Звено 5б: /healthz реально читает версию через internal/version.
	if !healthzUsesVersionPackage(t, root) {
		t.Error("cmd/gotcha/health.go: обработчик /healthz не импортирует internal/version " +
			"и не вызывает ни один из его экспортированных идентификаторов — цепочка версии " +
			"обрывается на потребителе, даже если Dockerfile/Makefile её донесли")
	}
}

// buildEnvVarsFromContract — истина звена 1: новые имена из envcontract.Renamed
// для старых имён envcontract.InfraOwned, отфильтрованные по префиксу
// GOTCHA_BUILD_ (остальные InfraOwned — GOTCHA_COMPOSE_*, отдельная
// подсистема, TestComposeVarsNamespaced уже проверяет её отдельно).
func buildEnvVarsFromContract(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, old := range envcontract.InfraOwned {
		newName, ok := envcontract.Renamed[old]
		if !ok {
			t.Fatalf("envcontract.InfraOwned содержит %q, которого нет среди ключей envcontract.Renamed — список рассинхронизирован", old)
		}
		if strings.HasPrefix(newName, "GOTCHA_BUILD_") {
			out[newName] = true
		}
	}
	return out
}

// dockerBuildEnvLineRe находит правую часть присваивания DOCKER_BUILD_ENV в
// Makefile; envAssignRe вытаскивает из неё имена переменных build-окружения.
var (
	dockerBuildEnvLineRe = regexp.MustCompile(`(?m)^DOCKER_BUILD_ENV\s*:=\s*(.+)$`)
	buildEnvAssignRe     = regexp.MustCompile(`\b(GOTCHA_BUILD_[A-Z0-9_]+)=`)
)

// makefileDockerBuildEnvVars разбирает строку `DOCKER_BUILD_ENV := ...` в
// Makefile и возвращает набор имён переменных, которые она выставляет.
func makefileDockerBuildEnvVars(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("Makefile: %v", err)
	}
	m := dockerBuildEnvLineRe.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("Makefile: не найдена строка `DOCKER_BUILD_ENV := ...` — обход ослеп")
	}
	out := map[string]bool{}
	for _, am := range buildEnvAssignRe.FindAllStringSubmatch(m[1], -1) {
		out[am[1]] = true
	}
	return out
}

// assertSameNameSet сравнивает два набора имён в обе стороны с понятным
// текстом ошибки, называющим и звено, и конкретное имя.
func assertSameNameSet(t *testing.T, link string, got, want map[string]bool) {
	t.Helper()
	for name := range want {
		if !got[name] {
			t.Errorf("%s: не выставляет %s — цепочка версии для этой переменной оборвана", link, name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s: выставляет %s, которой нет среди GOTCHA_BUILD_* envcontract.InfraOwned — "+
				"лишнее или устаревшее имя", link, name)
		}
	}
	if t.Failed() {
		var g, w []string
		for n := range got {
			g = append(g, n)
		}
		for n := range want {
			w = append(w, n)
		}
		sort.Strings(g)
		sort.Strings(w)
		t.Logf("%s: получено %v, ожидалось %v", link, g, w)
	}
}

// composeBuildArgsRoot — минимальный разбор docker-compose.yml, достаточный
// для чтения build.args сервиса gotcha: полная composeFile/composeService
// (compose_test.go) этой секции не знает, а заводить туда ещё одно поле
// ради одного теста — плодить связность между сторожами, которым и так есть
// что проверять порознь.
type composeBuildArgsRoot struct {
	Services map[string]struct {
		Build struct {
			Args map[string]string `yaml:"args"`
		} `yaml:"build"`
	} `yaml:"services"`
}

// composeBuildVarRe вытаскивает GOTCHA_BUILD_* из значения build-arg вида
// "${GOTCHA_BUILD_VERSION:-dev}".
var composeBuildVarRe = regexp.MustCompile(`\$\{(GOTCHA_BUILD_[A-Z0-9_]+)(?::[-?][^}]*)?\}`)

// composeBuildArgsForBuildVars читает build.args сервиса gotcha в
// docker-compose.yml и для каждого имени из buildVars находит ключ arg,
// чьё значение подставляет именно эту переменную. Возвращает пары
// «ключ arg → GOTCHA_BUILD_X» — вход звена 3.
func composeBuildArgsForBuildVars(t *testing.T, root string, buildVars map[string]bool) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("docker-compose.yml: %v", err)
	}
	var cf composeBuildArgsRoot
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("разбор docker-compose.yml: %v", err)
	}
	gotcha, ok := cf.Services["gotcha"]
	if !ok {
		t.Fatal("docker-compose.yml: нет сервиса gotcha — сторож ослеп")
	}
	if len(gotcha.Build.Args) == 0 {
		t.Fatal("docker-compose.yml: у сервиса gotcha пуст build.args — обход ослеп")
	}

	found := map[string]bool{}
	argKeyToBuildVar := map[string]string{}
	for argKey, value := range gotcha.Build.Args {
		m := composeBuildVarRe.FindStringSubmatch(value)
		if m == nil {
			continue
		}
		argKeyToBuildVar[argKey] = m[1]
		found[m[1]] = true
	}
	for name := range buildVars {
		if !found[name] {
			t.Errorf("docker-compose.yml: build.args сервиса gotcha не подставляет ${%s} ни в одном ключе — "+
				"цепочка версии для этой переменной обрывается на compose", name)
		}
	}
	return argKeyToBuildVar
}

// dockerfileARGRe находит объявления `ARG NAME` (со значением по умолчанию
// или без) на верхнем уровне Dockerfile.
var dockerfileARGRe = regexp.MustCompile(`(?m)^ARG\s+([A-Za-z_][A-Za-z0-9_]*)`)

func dockerfileARGNames(dockerfile string) map[string]bool {
	out := map[string]bool{}
	for _, m := range dockerfileARGRe.FindAllStringSubmatch(dockerfile, -1) {
		out[m[1]] = true
	}
	return out
}

// dockerfileServerBuildRe вырезает RUN-блок, который собирает серверный
// бинарь (-o /out/gotcha ./cmd/gotcha) — первый `go build` в Dockerfile,
// до того, как начинается кросс-сборка агента (`GOOS=linux GOARCH=... go
// build ... ./cmd/gotcha-agent`). Именно этот блок определяет, что видит
// /healthz: агентские бинарники версионируются для СВОЕГО потребителя
// (раздача через /agent/*), а не для /healthz сервера.
var dockerfileServerBuildRe = regexp.MustCompile(`(?s)RUN CGO_ENABLED=0 go build -mod=vendor.*?-o /out/gotcha \./cmd/gotcha`)

func dockerfileServerBuildBlock(t *testing.T, dockerfile string) string {
	t.Helper()
	m := dockerfileServerBuildRe.FindString(dockerfile)
	if m == "" {
		t.Fatal("Dockerfile: не найден RUN-блок сборки серверного бинаря (-o /out/gotcha ./cmd/gotcha) — обход ослеп")
	}
	return m
}

// dockerfileLdflagsRe находит `-X <importpath>/internal/version.<sym>=${<ARG>}`
// в тексте Dockerfile — ровно тот синтаксис, которым RUN-инструкции
// прокидывают build-arg в переменную уровня пакета через ldflags.
var dockerfileLdflagsRe = regexp.MustCompile(`-X\s+\S+/internal/version\.([A-Za-z_][A-Za-z0-9_]*)=\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// dockerfileLdflagsSymbols возвращает пары «ARG-ключ → символ internal/version»
// для каждого совпадения -X ...internal/version.sym=${ARG} в Dockerfile.
func dockerfileLdflagsSymbols(dockerfile string) map[string]string {
	out := map[string]string{}
	for _, m := range dockerfileLdflagsRe.FindAllStringSubmatch(dockerfile, -1) {
		sym, argKey := m[1], m[2]
		out[argKey] = sym
	}
	return out
}

// versionPackageVarExists — истинно, если internal/version/version.go
// объявляет переменную уровня пакета с именем sym (go/ast: `var` внутри
// GenDecl файла, а не любое совпадение имени идентификатора где угодно —
// иначе локальная переменная с тем же именем внутри функции создавала бы
// ложный зелёный).
func versionPackageVarExists(t *testing.T, root, sym string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "version", "version.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/version/version.go: %v", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == sym {
					return true
				}
			}
		}
	}
	return false
}

// healthzUsesVersionPackage — истинно, если cmd/gotcha/health.go импортирует
// internal/version и хотя бы раз вызывает один из его экспортированных
// идентификаторов (Version/Get/String/...). AST, а не грепом по тексту:
// импорт мог остаться неиспользуемым (Go бы не скомпилировался, но проверка
// импорта отдельно от проверки вызова всё равно точнее называет, какое
// именно звено порвано).
func healthzUsesVersionPackage(t *testing.T, root string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "gotcha", "health.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse imports cmd/gotcha/health.go: %v", err)
	}
	localName := ""
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != "gitflic.ru/otezvikentiy/gotcha/internal/version" {
			continue
		}
		if imp.Name != nil {
			localName = imp.Name.Name
		} else {
			localName = "version"
		}
	}
	if localName == "" {
		return false
	}

	full, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "gotcha", "health.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/gotcha/health.go: %v", err)
	}
	calls := false
	ast.Inspect(full, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != localName {
			return true
		}
		if ast.IsExported(sel.Sel.Name) {
			calls = true
		}
		return true
	})
	return calls
}
