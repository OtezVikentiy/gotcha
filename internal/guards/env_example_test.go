package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envReaderFuncs — функции cmd/gotcha/config.go, читающие переменные
// окружения. Имя переменной — первый строковый литерал GOTCHA_* в аргументах
// (у optionalBoolEnv имя стоит вторым аргументом, поэтому «первый аргумент»
// недостаточен). go/ast, а не регэксп — чтобы не ловить имена в текстах
// ошибок и комментариях.
var envReaderFuncs = map[string]bool{
	"str":             true,
	"intNum":          true,
	"num":             true,
	"boolEnv":         true,
	"boolEnvDef":      true,
	"optionalBoolEnv": true,
	"getenv":          true,
}

// collectGotchaEnvVars разбирает один Go-файл и возвращает имена переменных
// GOTCHA_*, читаемых через envReaderFuncs — тот же приём (go/ast, а не
// регэксп), что и раньше, вынесенный в функцию, потому что источников теперь
// два: cmd/gotcha/config.go (сервер) и internal/agent/config.go (агент,
// getenv-параметр LoadConfig подходит под то же имя "getenv"). Дублировать
// разбор под каждый источник — плодить два места, которые разъедутся
// синтаксисом так же, как разъехались сами копии контракта (см.
// agent_env_contract_test.go).
func collectGotchaEnvVars(t *testing.T, root, relFile string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relFile), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relFile, err)
	}
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if !envReaderFuncs[name] {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v := strings.Trim(lit.Value, `"`)
			if strings.HasPrefix(v, "GOTCHA_") {
				vars[v] = true
				break
			}
		}
		return true
	})
	return vars
}

// TestEnvExampleCoversConfig — №86: каждая переменная GOTCHA_*, которую
// читает cmd/gotcha/config.go ИЛИ internal/agent/config.go, обязана
// упоминаться в .env.example — единственном полном справочном файле
// переменных в репозитории. Исключений нет: переменные добавлялись в конфиг
// и не попадали в справочный файл повторно, класс закрывается сверкой, а не
// дисциплиной. internal/agent/config.go добавлен отдельно (аудит W3-G #2):
// восемь агентских переменных не были покрыты ни .env.example, ни этим
// сторожем — сторож видел только cmd/gotcha/config.go.
func TestEnvExampleCoversConfig(t *testing.T) {
	tree := Load(t)

	serverVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go"))
	if len(serverVars) < 20 {
		t.Fatalf("collected only %d server variables — cmd/gotcha/config.go parsing is broken", len(serverVars))
	}
	agentVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("internal", "agent", "config.go"))
	if len(agentVars) < 8 {
		t.Fatalf("collected only %d agent variables — internal/agent/config.go parsing is broken", len(agentVars))
	}

	vars := map[string]bool{}
	for v := range serverVars {
		vars[v] = true
	}
	for v := range agentVars {
		vars[v] = true
	}

	example, err := os.ReadFile(filepath.Join(tree.Root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for v := range vars {
		// Ищем «NAME=» (значением или закомментированным примером), а не голое
		// вхождение имени: короткое имя — префикс длинного
		// (GOTCHA_SSRF_ALLOW_PRIVATE ⊂ GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME), и
		// упоминание длинного давало бы ложный зелёный короткому.
		if !strings.Contains(string(example), v+"=") {
			t.Errorf("%s is read by config.go but missing from .env.example", v)
		}
	}
}
