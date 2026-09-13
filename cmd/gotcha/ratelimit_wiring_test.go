package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Методы существуют и покрыты тестами в internal/ingest, но раньше не вызывались
// из main.go: оператор не мог перенастроить лимит без пересборки бинаря.
func TestIngestRateLimitSettersWireConfigValues(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	fieldBySetter := map[string]string{
		"SetPreAuthRateLimit":     "PreAuthRateLimit",
		"SetSignalTouchRateLimit": "SignalTouchRateLimit",
	}
	calls := map[string]int{}
	hasField := map[string]bool{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		wantField, tracked := fieldBySetter[sel.Sel.Name]
		if !tracked {
			return true
		}
		calls[sel.Sel.Name]++
		for _, arg := range call.Args {
			ast.Inspect(arg, func(an ast.Node) bool {
				if s, ok := an.(*ast.SelectorExpr); ok && s.Sel.Name == wantField {
					hasField[sel.Sel.Name] = true
				}
				return true
			})
		}
		return true
	})

	for setter, field := range fieldBySetter {
		if calls[setter] != 1 {
			t.Errorf("вызовов %s в main.go = %d, want 1", setter, calls[setter])
		}
		if !hasField[setter] {
			t.Errorf("%s в main.go не читает cfg.%s — лимит не настраивается из окружения", setter, field)
		}
	}
}
