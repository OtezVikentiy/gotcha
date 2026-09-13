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

const webhookKindRegistryFile = "internal/notify/kind.go"

// collectRegistryKinds reads the const block in kind.go — the single place a
// webhook kind value is allowed to be a literal (see webhook_kind_form_test.go).
func collectRegistryKinds(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	kinds := map[string]bool{}
	for _, gf := range tree.GoFiles {
		if gf.Path != webhookKindRegistryFile {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Kind") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				kinds[unquoteGoString(lit.Value)] = true
			}
			return true
		})
	}
	return kinds
}

func unquoteGoString(s string) string { return strings.Trim(s, `"`) }

// collectRegistryKindNames reads the const NAMES (not values) from kind.go.
func collectRegistryKindNames(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, gf := range tree.GoFiles {
		if gf.Path != webhookKindRegistryFile {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Kind") {
					names[name.Name] = true
				}
			}
			return true
		})
	}
	return names
}

// collectRedactedKindKeyNames reads the redactedKindKeys map's key IDENTIFIERS
// (KindNewIssue, ...), not their string values.
func collectRedactedKindKeyNames(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, gf := range tree.GoFiles {
		if gf.Path != "internal/notify/redact.go" {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if name.Name != "redactedKindKeys" || i >= len(vs.Values) {
					continue
				}
				comp, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range comp.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					id, ok := kv.Key.(*ast.Ident)
					if !ok {
						pos := fset.Position(kv.Key.Pos())
						t.Errorf("%s:%d: redactedKindKeys key is not a registry constant reference", pos.Filename, pos.Line)
						continue
					}
					names[id.Name] = true
				}
			}
			return true
		})
	}
	return names
}

// KindChannelTest не проходит через приватность намеренно — единственное
// разрешённое исключение из полноты redactedKindKeys относительно реестра.
const registryKindWithoutRedaction = "KindChannelTest"

// Каждая константа реестра (кроме KindChannelTest) обязана быть ключом
// redactedKindKeys — иначе новый вид молча минует приватность внешних каналов.
func TestRedactedKindKeysCoverRegistry(t *testing.T) {
	tree := Load(t)
	registry := collectRegistryKindNames(t, tree)
	if len(registry) < 15 {
		t.Fatalf("blind guard: found only %d constants in %s — the scanner is broken", len(registry), webhookKindRegistryFile)
	}
	delete(registry, registryKindWithoutRedaction)

	redacted := collectRedactedKindKeyNames(t, tree)
	if len(redacted) < 15 {
		t.Fatalf("blind guard: found only %d keys in redactedKindKeys — the scanner is broken", len(redacted))
	}

	for name := range registry {
		if !redacted[name] {
			t.Errorf("notify.%s is in the registry but missing from redactedKindKeys", name)
		}
	}
	for name := range redacted {
		if !registry[name] {
			t.Errorf("redactedKindKeys has %q, which is not a registry constant", name)
		}
	}
}

// Каждый вид в реестре обязан быть назван в разделе «Формат тела вебхука»
// обеих локалей.
func TestWebhookKindsDocumented(t *testing.T) {
	tree := Load(t)
	kinds := collectRegistryKinds(t, tree)
	if len(kinds) < 15 {
		t.Fatalf("blind guard: found only %d kinds in %s — the scanner is broken", len(kinds), webhookKindRegistryFile)
	}

	for _, lang := range []string{"ru", "en"} {
		docPath := filepath.Join(tree.Root, "internal", "docs", lang, "alerts.md")
		doc, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc)
		for kind := range kinds {
			needle := "`" + kind + "`"
			if !strings.Contains(body, needle) {
				t.Errorf("%s: webhook kind %q is in the registry but not named in alerts.md", lang, kind)
			}
		}
	}
}
