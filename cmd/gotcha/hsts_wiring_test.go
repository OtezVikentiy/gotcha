package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
)

// порядок аргументов web.HSTSHeaderValue следует её сигнатуре; перестановка последних двух
// компилируется молча, но отправит preload без includeSubDomains
var wantHSTSHeaderValueArgs = []string{
	"HSTSEnabled", "HSTSMaxAgeSeconds", "HSTSIncludeSubDomains", "HSTSPreload",
}

// guards/handlerassembly_test.go не ловит эту проводку: он считает поле покрытым, если оно
// установлено хоть в New, хоть в main.go — не то, что main.go ПЕРЕЗАПИСЫВАЕТ дефолт из конфига
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

		if len(call.Args) != len(wantHSTSHeaderValueArgs) {
			t.Errorf("web.HSTSHeaderValue(...) в main.go вызван с %d аргументами, want %d",
				len(call.Args), len(wantHSTSHeaderValueArgs))
			return false
		}
		gotArgs := make([]string, len(call.Args))
		for i, arg := range call.Args {
			sel, ok := arg.(*ast.SelectorExpr)
			if !ok {
				t.Errorf("web.HSTSHeaderValue(...): аргумент %d — не селектор вида cfg.Поле (%T)", i, arg)
				continue
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "cfg" {
				t.Errorf("web.HSTSHeaderValue(...): аргумент %d — не поле cfg (%s.%s)",
					i, sel.X, sel.Sel.Name)
				continue
			}
			gotArgs[i] = sel.Sel.Name
		}
		if !reflect.DeepEqual(gotArgs, wantHSTSHeaderValueArgs) {
			t.Errorf("web.HSTSHeaderValue(...) в main.go вызван с cfg.%v, want cfg.%v — "+
				"перепутанный порядок аргументов даёт РАБОЧИЙ, но неверный заголовок "+
				"(например includeSubDomains и preload переставлены местами шлют preload "+
				"без includeSubDomains) без единой ошибки сборки",
				gotArgs, wantHSTSHeaderValueArgs)
		}

		return false
	})

	if !found {
		t.Error("main.go не присваивает webHandler.HSTSHeader = web.HSTSHeaderValue(...) — " +
			"GOTCHA_HSTS_* перестанут влиять на заголовок, инстанс молча останется на " +
			"историческом дефолте web.New(), а internal/guards/handlerassembly_test.go " +
			"это не ловит (поле уже покрыто дефолтом New)")
	}
}
