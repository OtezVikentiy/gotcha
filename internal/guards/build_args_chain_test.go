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

func TestBuildArgsChainWired(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	buildVars := buildEnvVarsFromContract(t)
	if len(buildVars) < 3 {
		t.Fatalf("обход ослеп: envcontract.InfraOwned содержит только %d имён GOTCHA_BUILD_*, ожидалось ≥3", len(buildVars))
	}

	makefileVars := makefileDockerBuildEnvVars(t, root)
	assertSameNameSet(t, "Makefile: DOCKER_BUILD_ENV", makefileVars, buildVars)

	argKeyToBuildVar := composeBuildArgsForBuildVars(t, root, buildVars)

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

	// -X ищем в RUN-блоке серверной сборки (/out/gotcha), не агента: те же
	// три символа встречаются и в сборке gotcha-agent.
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

		if !versionPackageVarExists(t, root, sym) {
			t.Errorf("internal/version: -ldflags Dockerfile целится в internal/version.%s (ARG %s ← %s), "+
				"но такой переменной уровня пакета в internal/version/version.go нет", sym, argKey, buildVar)
		}
	}

	if !healthzUsesVersionPackage(t, root) {
		t.Error("cmd/gotcha/health.go: обработчик /healthz не импортирует internal/version " +
			"и не вызывает ни один из его экспортированных идентификаторов — цепочка версии " +
			"обрывается на потребителе, даже если Dockerfile/Makefile её донесли")
	}
}

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

var (
	dockerBuildEnvLineRe = regexp.MustCompile(`(?m)^DOCKER_BUILD_ENV\s*:=\s*(.+)$`)
	buildEnvAssignRe     = regexp.MustCompile(`\b(GOTCHA_BUILD_[A-Z0-9_]+)=`)
)

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

// Минимальный разбор build.args: заводить сюда поля полного composeFile
// (compose_test.go) ради одного теста незачем.
type composeBuildArgsRoot struct {
	Services map[string]struct {
		Build struct {
			Args map[string]string `yaml:"args"`
		} `yaml:"build"`
	} `yaml:"services"`
}

var composeBuildVarRe = regexp.MustCompile(`\$\{(GOTCHA_BUILD_[A-Z0-9_]+)(?::[-?][^}]*)?\}`)

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

var dockerfileARGRe = regexp.MustCompile(`(?m)^ARG\s+([A-Za-z_][A-Za-z0-9_]*)`)

func dockerfileARGNames(dockerfile string) map[string]bool {
	out := map[string]bool{}
	for _, m := range dockerfileARGRe.FindAllStringSubmatch(dockerfile, -1) {
		out[m[1]] = true
	}
	return out
}

// Берёт RUN серверной сборки (-o /out/gotcha), не кросс-сборки агента —
// у неё свои -X для /agent/*.
var dockerfileServerBuildRe = regexp.MustCompile(`(?s)RUN CGO_ENABLED=0 go build -mod=vendor.*?-o /out/gotcha \./cmd/gotcha`)

func dockerfileServerBuildBlock(t *testing.T, dockerfile string) string {
	t.Helper()
	m := dockerfileServerBuildRe.FindString(dockerfile)
	if m == "" {
		t.Fatal("Dockerfile: не найден RUN-блок сборки серверного бинаря (-o /out/gotcha ./cmd/gotcha) — обход ослеп")
	}
	return m
}

var dockerfileLdflagsRe = regexp.MustCompile(`-X\s+\S+/internal/version\.([A-Za-z_][A-Za-z0-9_]*)=\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func dockerfileLdflagsSymbols(dockerfile string) map[string]string {
	out := map[string]string{}
	for _, m := range dockerfileLdflagsRe.FindAllStringSubmatch(dockerfile, -1) {
		sym, argKey := m[1], m[2]
		out[argKey] = sym
	}
	return out
}

// var внутри GenDecl файла, а не любое совпадение имени — иначе локальная
// переменная той же функции давала бы ложный зелёный.
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

// AST, не грепом: важен реальный вызов, а не просто упоминание в импорте.
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
