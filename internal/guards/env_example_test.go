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

// TestEnvExampleCoversConfig — №86: каждая переменная GOTCHA_*, которую
// читает cmd/gotcha/config.go, обязана упоминаться в .env.example —
// единственном полном справочном файле переменных в репозитории. Исключений
// нет: переменные добавлялись в конфиг и не попадали в справочный файл
// повторно, класс закрывается сверкой, а не дисциплиной.
func TestEnvExampleCoversConfig(t *testing.T) {
	tree := Load(t)
	fset := token.NewFileSet()
	src := filepath.Join(tree.Root, "cmd", "gotcha", "config.go")
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
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
	if len(vars) < 20 {
		t.Fatalf("collected only %d variables — config.go parsing is broken", len(vars))
	}
	example, err := os.ReadFile(filepath.Join(tree.Root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for v := range vars {
		if !strings.Contains(string(example), v) {
			t.Errorf("%s is read by config.go but missing from .env.example", v)
		}
	}
}
