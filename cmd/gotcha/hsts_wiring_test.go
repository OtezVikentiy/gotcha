package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
)

// wantHSTSHeaderValueArgs — порядок параметров web.HSTSHeaderValue(enabled
// bool, maxAgeSeconds int, includeSubDomains, preload bool) — см. её
// сигнатуру в internal/web/hsts.go. Перестановка двух последних
// (includeSubDomains ↔ preload) компилируется молча: оба bool, оба cfg-поля
// существуют — но инстанс с INCLUDE_SUBDOMAINS=true, PRELOAD=false отдал бы
// заголовок с токеном preload, ровно ту комбинацию, ради запрета которой
// написан блок валидации в config.go. Матчить сам факт вызова
// web.HSTSHeaderValue(...), не заглядывая внутрь, эту перестановку не ловит.
var wantHSTSHeaderValueArgs = []string{
	"HSTSEnabled", "HSTSMaxAgeSeconds", "HSTSIncludeSubDomains", "HSTSPreload",
}

// TestHSTSHeaderWiredFromConfig — main.go обязан проставлять
// webHandler.HSTSHeader вызовом web.HSTSHeaderValue(cfg.HSTS...) с полями В
// ПРАВИЛЬНОМ ПОРЯДКЕ (wantHSTSHeaderValueArgs), а не оставлять его на
// историческом дефолте "max-age=31536000", зашитом в web.New(...)
// (internal/web/web.go:507, тот же приём, что у RegistrationMode).
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
