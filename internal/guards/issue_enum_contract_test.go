package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Ловит только группу из 2+ канонических литералов в одном AST-узле, не разбитую по
// case-веткам копию и не файл без буквального импорта internal/issue.
var canonStatusLiterals = map[string]bool{"unresolved": true, "resolved": true, "ignored": true}
var canonLevelLiterals = map[string]bool{"debug": true, "info": true, "warning": true, "error": true, "fatal": true}

const issueImportPath = `"gitflic.ru/otezvikentiy/gotcha/internal/issue"`

var issueEnumExcludedDirs = []string{
	filepath.Join("internal", "issue") + string(filepath.Separator),
}

type issueEnumViolation struct {
	file    string
	line    int
	domain  string
	literal []string
}

func issueEnumCandidateFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	internalDir := filepath.Join(root, "internal")
	err := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, excl := range issueEnumExcludedDirs {
			if strings.HasPrefix(rel, excl) {
				return nil
			}
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), issueImportPath) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход internal/: %v", err)
	}
	sort.Strings(out)
	return out
}

func literalGroupLiterals(elts []ast.Expr) []string {
	var out []string
	for _, e := range elts {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					out = append(out, s)
				}
			}
		case *ast.KeyValueExpr:
			if lit, ok := v.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					out = append(out, s)
				}
			}
			if lit, ok := v.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Оба домена сразу не бывают: множества не пересекаются лексически.
func classifyLiteralGroup(lits []string) (domain string, found []string) {
	var statusFound, levelFound []string
	seen := map[string]bool{}
	for _, l := range lits {
		if seen[l] {
			continue
		}
		if canonStatusLiterals[l] {
			statusFound = append(statusFound, l)
			seen[l] = true
		} else if canonLevelLiterals[l] {
			levelFound = append(levelFound, l)
			seen[l] = true
		}
	}
	if len(statusFound) >= 2 {
		return "status", statusFound
	}
	if len(levelFound) >= 2 {
		return "level", levelFound
	}
	return "", nil
}

func scanFileForIssueEnumCopies(t *testing.T, root, rel string) []issueEnumViolation {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, rel)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", rel, err)
	}
	var out []issueEnumViolation
	ast.Inspect(f, func(n ast.Node) bool {
		var lits []string
		var pos token.Pos
		switch v := n.(type) {
		case *ast.CompositeLit:
			lits = literalGroupLiterals(v.Elts)
			pos = v.Pos()
		case *ast.CaseClause:
			lits = literalGroupLiterals(v.List)
			pos = v.Pos()
		default:
			return true
		}
		domain, found := classifyLiteralGroup(lits)
		if domain == "" {
			return true
		}
		p := fset.Position(pos)
		out = append(out, issueEnumViolation{file: rel, line: p.Line, domain: domain, literal: found})
		return true
	})
	return out
}

func TestNoIssueEnumLiteralCopies(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	files := issueEnumCandidateFiles(t, root)
	// Порог ниже фактического (запас на рефакторинг); падение ниже 5 значит,
	// что сканер перестал находить файлы, а не что потребителей стало меньше.
	if len(files) < 5 {
		t.Fatalf("обход ослеп: файлов, импортирующих internal/issue, найдено %d (меньше 5) — проверить issueEnumCandidateFiles", len(files))
	}

	var violations []issueEnumViolation
	for _, rel := range files {
		violations = append(violations, scanFileForIssueEnumCopies(t, root, rel)...)
	}
	for _, v := range violations {
		t.Errorf("%s:%d — собственный литеральный перечень issue.%s (%s) вместо issue.Status*/issue.Level*/issue.IsValidStatus/issue.IsValidLevel",
			v.file, v.line, v.domain, strings.Join(v.literal, ", "))
	}
}

// internal/trace/perfissue.go держит свой независимый перечень статусов той
// же формы и не должен попадать в кандидаты — он не импортирует internal/issue.
func TestIssueEnumScanExcludesSiblingContract(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	files := issueEnumCandidateFiles(t, root)
	want := filepath.Join("internal", "trace", "perfissue.go")
	for _, f := range files {
		if f == want {
			t.Fatalf("%s попал в кандидаты сканирования — он не импортирует internal/issue и держит независимый контракт (perf_issues.status), а не копию issue.Status*", want)
		}
	}
}

// Для этих функций любое, даже одиночное вхождение канона — нарушение: они
// разбиты по отдельным case-веткам, и порог ≥2-в-узле их не ловит.
var issueEnumStrictFuncs = map[string]bool{
	"statusBadgeClass": true,
	"issueStatusLabel": true,
}

func scanFuncForAnyIssueEnumLiteral(t *testing.T, root, rel string) []issueEnumViolation {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, rel)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", rel, err)
	}
	var out []issueEnumViolation
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !issueEnumStrictFuncs[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			var domain string
			switch {
			case canonStatusLiterals[s]:
				domain = "status"
			case canonLevelLiterals[s]:
				domain = "level"
			default:
				return true
			}
			p := fset.Position(lit.Pos())
			out = append(out, issueEnumViolation{file: rel, line: p.Line, domain: domain, literal: []string{s}})
			return true
		})
	}
	return out
}

func TestIssueStatusBadgeFuncsRejectAnyRawLiteral(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("internal", "web", "templates", "issues_templ.go")
	if _, statErr := os.Stat(filepath.Join(root, rel)); statErr != nil {
		t.Fatalf("%s не найден — сгенерирован ли шаблон (make templ)? %v", rel, statErr)
	}
	violations := scanFuncForAnyIssueEnumLiteral(t, root, rel)
	for _, v := range violations {
		t.Errorf("%s:%d — сырой литерал issue.%s (%s) в функции из issueEnumStrictFuncs вместо issue.Status*/issue.Level*",
			v.file, v.line, v.domain, strings.Join(v.literal, ", "))
	}
}
