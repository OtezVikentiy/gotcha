package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// go/ast, не литерал трёх имён: копия списка ролей в тесте не заметила бы
// новую роль, добавленную в internal/org/member.go, — сверяла бы копию с копией.
func orgRoleValues(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "org", "member.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/org/member.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Role" {
				continue
			}
			for _, val := range vs.Values {
				lit, ok := val.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out = append(out, s)
			}
		}
	}
	if len(out) < 3 {
		t.Fatalf("нашли %d констант типа Role в internal/org/member.go — сканер сломан, а не ролей стало меньше", len(out))
	}
	return out
}

func sortedCopy(vals []string) []string {
	out := append([]string(nil), vals...)
	sort.Strings(out)
	return out
}

// Обе стороны первичны — тип управляет кодом, CHECK управляет данными в БД —
// поэтому сверяются друг с другом напрямую, а не по общей схеме «копия vs истина».
func TestOrgRolesTypeMatchesSchema(t *testing.T) {
	tree := Load(t)
	code := sortedCopy(orgRoleValues(t, tree.Root))
	schema := sortedCopy(checkInValues(t, migrationBody(t, tree, "0002_tenancy.up.sql"), "role"))
	if len(code) != len(schema) {
		t.Fatalf("org.Role даёт %v, CHECK схемы даёт %v — множества разной длины", code, schema)
	}
	for i := range code {
		if code[i] != schema[i] {
			t.Fatalf("org.Role даёт %v, CHECK схемы даёт %v — множества расходятся", code, schema)
		}
	}
}

func orgTeamsDocBody(t *testing.T, tree *Tree, lang string) string {
	t.Helper()
	path := filepath.Join(tree.Root, "internal", "docs", lang, "teams.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	return string(body)
}

// Границы \b — иначе "admin" зеленеет от "administrator", а "member" от "members".
func roleMentioned(body, role string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(role) + `\b`)
	return re.MatchString(body)
}

func TestOrgRolesDocumented(t *testing.T) {
	tree := Load(t)
	for _, lang := range []string{"ru", "en"} {
		body := orgTeamsDocBody(t, tree, lang)
		for _, role := range orgRoleValues(t, tree.Root) {
			if !roleMentioned(body, role) {
				t.Errorf("internal/docs/%s/teams.md: роль %q из org.Role не упомянута отдельным словом", lang, role)
			}
		}
	}
}
