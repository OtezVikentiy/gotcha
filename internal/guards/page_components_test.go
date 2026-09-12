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

const (
	templatesDir         = "internal/web/templates"
	pageComponentsFile   = "internal/web/templates/errorpaths_test.go"
	pageComponentsFunc   = "pageComponents"
	templComponentPkg    = "templ"
	templComponentTypeID = "Component"
)

var pageComponentExceptions = map[string]string{}

const wantExceptions = 0

func TestPageComponentsMapComplete(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	declared := scanDeclaredPageComponents(t, root)
	if len(declared) < 50 {
		t.Fatalf("обход _templ.go ослеп: страничных компонентов найдено %d (ожидалось не меньше 50) — сломан сам сторож, а не проверяемый код", len(declared))
	}

	if len(pageComponentExceptions) != wantExceptions {
		t.Fatalf("pageComponentExceptions: %d записей, а сторож ждёт %d — список исключений изменился. "+
			"Если страница осознанно не входит в pageComponents(), опиши причину строкой в pageComponentExceptions "+
			"и подними wantExceptions на новое число; если нет — построй честную фикстуру и не исключай компонент",
			len(pageComponentExceptions), wantExceptions)
	}
	for name := range pageComponentExceptions {
		if !declared[name] {
			t.Errorf("pageComponentExceptions содержит %q — такого страничного компонента нет среди экспортированных функций %s, похоже на опечатку или устаревшую запись", name, templatesDir)
		}
	}

	used := scanUsedPageComponents(t, root)

	for name := range declared {
		if pageComponentExceptions[name] != "" {
			continue
		}
		if !used[name] {
			t.Errorf("страничный компонент %q объявлен в %s, но не вызывается в %s() (%s) — "+
				"TestRenderPropagatesWriteErrors/TestRenderRespectsCancelledContext на нём не проверяются",
				name, templatesDir, pageComponentsFunc, pageComponentsFile)
		}
	}
}

func scanDeclaredPageComponents(t *testing.T, root string) map[string]bool {
	t.Helper()
	dir := filepath.Join(root, templatesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("чтение %s: %v", templatesDir, err)
	}

	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_templ.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", e.Name(), err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !fd.Name.IsExported() {
				continue
			}
			if !returnsTemplComponent(fd) {
				continue
			}
			declared[fd.Name.Name] = true
		}
	}
	return declared
}

func returnsTemplComponent(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
		return false
	}
	sel, ok := fd.Type.Results.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == templComponentPkg && sel.Sel.Name == templComponentTypeID
}

func scanUsedPageComponents(t *testing.T, root string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, pageComponentsFile)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", pageComponentsFile, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Recv == nil && fd.Name.Name == pageComponentsFunc {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s: не найдена функция %s — сторож ослеп, а не код исправился", pageComponentsFile, pageComponentsFunc)
	}

	used := map[string]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		cl, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if _, ok := kv.Key.(*ast.BasicLit); !ok {
				continue
			}
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok {
				continue
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				continue
			}
			used[id.Name] = true
		}
		return true
	})
	if len(used) == 0 {
		t.Fatalf("%s: в %s() не нашлось ни одной записи вида \"Ключ\": Конструктор(...) — сторож ослеп, а не карта опустела", pageComponentsFile, pageComponentsFunc)
	}
	return used
}
