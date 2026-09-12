package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

const (
	handlerWebGoFile     = "internal/web/web.go"
	handlerMainGoFile    = "cmd/gotcha/main.go"
	handlerNewFuncName   = "New"
	handlerTypeIdentName = "Handler"
	handlerVarInMain     = "webHandler"
)

var indirectSetHandlerFields = map[string]string{
	"pages":  "заполняется внутри Handler.Register(mux) (internal/web/web.go:882), вызываемого из cmd/gotcha/server.go:62 при старте сервера — main.go не может присвоить его напрямую (поле неэкспортируемое)",
	"routes": "заполняется внутри Handler.Register(mux) (internal/web/web.go:883), тем же вызовом, что и pages",
}

var zeroValueHandlerFields = map[string]string{
	"agentETags":          "sync.Map — нулевое значение готово к работе (agentdist.go, ленивый ETag-кеш бинарей агента)",
	"ssoProviders":        "ssoCache — нулевое значение готово к работе (sso.go, process-local кеш per-org OIDC-провайдеров)",
	"statusCache":         "statusCache — 30-секундный кеш публичных статус-страниц, нулевое значение готово к работе (statuspage.go)",
	"crossOriginRejected": "atomic.Int64 — нулевое значение готово к работе (crossorigin.go, счётчик отказов same-origin)",
	"coThrottle":          "coThrottle — нулевое значение готово к работе (crossorigin.go, троттлинг лога отказов)",
}

// Присвоение внутри `if ... { ... }` засчитывается наравне с безусловным —
// новое поле без nil-чека на месте использования сторож пропустит молча.
func TestHandlerAssemblyComplete(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatalf("поиск корня репозитория: %v", err)
	}

	allFields := handlerFieldNames()
	newFields := parseHandlerNewFields(t, root)
	mainFields := parseHandlerMainAssignFields(t, root)

	for name, reason := range indirectSetHandlerFields {
		if !allFields[name] {
			t.Fatalf("indirectSetHandlerFields[%q] (%s) — такого поля больше нет у web.Handler, запись устарела", name, reason)
		}
	}
	for name, reason := range zeroValueHandlerFields {
		if !allFields[name] {
			t.Fatalf("zeroValueHandlerFields[%q] (%s) — такого поля больше нет у web.Handler, запись устарела", name, reason)
		}
	}

	for name := range allFields {
		if newFields[name] || mainFields[name] {
			continue
		}
		if _, ok := indirectSetHandlerFields[name]; ok {
			continue
		}
		if _, ok := zeroValueHandlerFields[name]; ok {
			continue
		}
		t.Errorf("web.Handler.%s не устанавливается ни New(...), ни присвоением в %s, не значится в indirectSetHandlerFields и не значится в zeroValueHandlerFields — забытое поле, при обычном старте раздел молча ответит 404 вместо ошибки сборки", name, handlerMainGoFile)
	}

	for name := range mainFields {
		if !allFields[name] {
			t.Errorf("%s присваивает web.Handler.%s, но такого поля больше нет в структуре — присвоение устарело, удалить его из main.go", handlerMainGoFile, name)
		}
	}
	for name := range newFields {
		if !allFields[name] {
			t.Errorf("%s: %s(...) присваивает Handler.%s, но такого поля больше нет в структуре", handlerWebGoFile, handlerNewFuncName, name)
		}
	}
}

func handlerFieldNames() map[string]bool {
	typ := reflect.TypeOf(web.Handler{})
	set := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		set[typ.Field(i).Name] = true
	}
	return set
}

// ast.Inspect, а не разбор ReturnStmt — переживает промежуточную переменную
// (`h := &Handler{...}; return h`) без переписывания сторожа.
func parseHandlerNewFields(t *testing.T, root string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, handlerWebGoFile)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", handlerWebGoFile, err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Recv == nil && fd.Name.Name == handlerNewFuncName {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s: не найдена функция %s(...) — сторож ослеп, а не код исправился", handlerWebGoFile, handlerNewFuncName)
	}

	set := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := cl.Type.(*ast.Ident)
		if !ok || id.Name != handlerTypeIdentName {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			set[key.Name] = true
		}
		return true
	})
	if len(set) == 0 {
		t.Fatalf("%s: %s(...) не содержит ни одного композитного литерала %s{...} с присвоенными полями — сторож ослеп", handlerWebGoFile, handlerNewFuncName, handlerTypeIdentName)
	}
	return set
}

// Вызов метода или чтение через селектор не считается присвоением — это не
// *ast.AssignStmt, а *ast.ExprStmt/аргумент вызова.
func parseHandlerMainAssignFields(t *testing.T, root string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, handlerMainGoFile)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", handlerMainGoFile, err)
	}

	set := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN {
			return true
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != handlerVarInMain {
				continue
			}
			set[sel.Sel.Name] = true
		}
		return true
	})
	if len(set) == 0 {
		t.Fatalf("%s: не найдено ни одного присвоения %s.<Поле> = ... — сторож ослеп, а не main.go изменился", handlerMainGoFile, handlerVarInMain)
	}
	return set
}
