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

// TestSelfMetricsDocumented — №126: каждая метрика, зарегистрированная в
// selfmetrics (вызовы Add/AddInt с selfmetrics.Counter|Gauge первым
// аргументом), обязана упоминаться в self-monitoring.md ОБОИХ языков.
// Пятиаргументная форма с селектором пакета отличает регистрации от прочих
// методов Add по дереву (Profiles.Add(projectID, p) и т.п.). Имя обязано
// быть строковым литералом — иначе сверка невозможна и регистрация выпала
// бы из неё молча.
func TestSelfMetricsDocumented(t *testing.T) {
	tree := Load(t)
	fset := token.NewFileSet()
	names := map[string][]string{} // имя метрики → файлы регистрации
	for _, gf := range tree.GoFiles {
		if gf.Generated || strings.HasSuffix(gf.Path, "_test.go") ||
			strings.HasPrefix(gf.Path, "internal/guards/") {
			continue
		}
		if !strings.Contains(gf.Body, "selfmetrics.") {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 5 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Add" && sel.Sel.Name != "AddInt") {
				return true
			}
			typ, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || pkg.Name != "selfmetrics" {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: metric name must be a string literal (call at %s)",
					gf.Path, fset.Position(call.Pos()))
				return true
			}
			name := strings.Trim(lit.Value, `"`)
			names[name] = append(names[name], gf.Path)
			return true
		})
	}
	if len(names) < 10 {
		t.Fatalf("collected only %d metrics — the scanner is broken", len(names))
	}
	for _, lang := range []string{"en", "ru"} {
		doc, err := os.ReadFile(filepath.Join(tree.Root, "internal", "docs", lang, "self-monitoring.md"))
		if err != nil {
			t.Fatal(err)
		}
		for name, files := range names {
			if !strings.Contains(string(doc), name) {
				t.Errorf("%s is registered (%s) but missing from %s/self-monitoring.md",
					name, strings.Join(files, ", "), lang)
			}
		}
	}
}
