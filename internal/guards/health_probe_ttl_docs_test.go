package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	healthProbeFile     = "cmd/gotcha/health.go"
	healthProbeTTLConst = "healthProbeTTL"
)

// Число в прозе self-monitoring.md (обе локали) против healthProbeTTL в cmd/gotcha/health.go.
func TestSelfMonitoringDocMatchesHealthProbeTTL(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, healthProbeFile), nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", healthProbeFile, err)
	}

	seconds, ok := durationConstSeconds(f, healthProbeTTLConst)
	if !ok {
		t.Fatalf("%s: не найдена константа %s вида N * time.Second — сторож ослеп, а не код исправился",
			healthProbeFile, healthProbeTTLConst)
	}

	for _, doc := range []struct {
		locale string
		path   string
		phrase string
	}{
		{"ru", "internal/docs/ru/self-monitoring.md", fmt.Sprintf("раза в %d секунд", seconds)},
		{"en", "internal/docs/en/self-monitoring.md", fmt.Sprintf("every %d seconds", seconds)},
	} {
		doc := doc
		t.Run(doc.locale, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), doc.phrase) {
				t.Errorf("%s: не нашёл %q рядом с описанием общего кэша /healthz и /readyz.\n"+
					"%s = %d (%s) разошлась с числом в прозе — поправить прозу под текущий TTL.",
					doc.path, doc.phrase, healthProbeTTLConst, seconds, healthProbeFile)
			}
		})
	}
}

// healthProbeTTL объявлена как `N * time.Second` — BasicLit-парсер intConst её не берёт.
func durationConstSeconds(f *ast.File, name string) (int, bool) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				bin, ok := vs.Values[i].(*ast.BinaryExpr)
				if !ok || bin.Op != token.MUL {
					continue
				}
				lit, ok := bin.X.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				sel, ok := bin.Y.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "time" || sel.Sel.Name != "Second" {
					continue
				}
				n, err := strconv.Atoi(lit.Value)
				if err != nil {
					continue
				}
				return n, true
			}
		}
	}
	return 0, false
}
