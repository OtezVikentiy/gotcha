package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestHSTSHeaderWiredFromConfig — main.go обязан проставлять
// webHandler.HSTSHeader вызовом web.HSTSHeaderValue(cfg.HSTS...), а не
// оставлять его на историческом дефолте "max-age=31536000", зашитом в
// web.New(...) (internal/web/web.go:507, тот же приём, что у RegistrationMode).
//
// internal/guards/handlerassembly_test.go эту проводку не ловит: его методика
// (докблок TestHandlerAssemblyComplete) считает поле покрытым, если оно
// установлено ХОТЯ БЫ в одном из двух мест — New ИЛИ main.go, — а не то, что
// main.go реально ПЕРЕЗАПИСЫВАЕТ дефолт значением из конфига. HSTSHeader уже
// "покрыт" дефолтом в New, поэтому удаление строки проводки не роняет тот
// сторож — четыре переменные GOTCHA_HSTS_* молча перестают на что-либо
// влиять, инстанс навсегда остаётся на историческом дефолте, и ни один
// существующий тест (ни guards, ни cmd/gotcha) этого не замечает без этого
// теста.
//
// Разбирает main.go напрямую через go/ast (без типизации, без БД, без
// запуска run()) — тот же приём, что TestRewrapAllSecretsCallSiteOrder
// (rewrap_bootstrap_test.go).
func TestHSTSHeaderWiredFromConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		lhs, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		lhsRecv, ok := lhs.X.(*ast.Ident)
		if !ok || lhsRecv.Name != "webHandler" || lhs.Sel.Name != "HSTSHeader" {
			return true
		}

		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		callFn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		callPkg, ok := callFn.X.(*ast.Ident)
		if !ok || callPkg.Name != "web" || callFn.Sel.Name != "HSTSHeaderValue" {
			return true
		}

		found = true
		return false
	})

	if !found {
		t.Error("main.go не присваивает webHandler.HSTSHeader = web.HSTSHeaderValue(...) — " +
			"GOTCHA_HSTS_* перестанут влиять на заголовок, инстанс молча останется на " +
			"историческом дефолте web.New(), а internal/guards/handlerassembly_test.go " +
			"это не ловит (поле уже покрыто дефолтом New)")
	}
}
