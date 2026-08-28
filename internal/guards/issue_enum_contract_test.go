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

// Контракт статусов/уровней issue живёт в internal/issue/query.go —
// StatusXxx/Statuses и LevelXxx/Levels (issue.IsValidStatus/IsValidLevel —
// сверка с ними). Раньше набор был размножен литералами по потребителям:
// internal/web/issues.go (дефолт фильтра списка и bulkActionStatus),
// internal/web/exports.go (exportValidStatuses/exportValidLevels) держали
// собственные копии трёх/пяти строк вместо обращения к issue — ни одна из
// копий не сверялась ни с issue, ни друг с другом. internal/ingest/
// sentry.go был исключением: его validLevels уже строился ИЗ
// issue.LevelXxx, а не из собственных литералов, — образец, по которому
// консолидированы остальные потребители.
//
// Этот сторож ловит РЕЦИДИВ: появление в потребителе issue собственного
// литерального перечня статусов/уровней вместо ссылки на issue.StatusXxx/
// issue.LevelXxx/issue.IsValidStatus/issue.IsValidLevel.
//
// Область сканирования — файлы internal/**, ИМПОРТИРУЮЩИЕ internal/issue
// (issueImportPath), кроме:
//   - самого internal/issue — он владелец, его литералы и есть канон;
//   - *_test.go — тестовые фикстуры сплошь и рядом заводят
//     issue.Issue{Status: "resolved"} или сравнивают тело ответа с
//     литералом ассерта; это не «копия перечня для валидации», а данные
//     теста, и их массовая правка — не предмет этой задачи.
//
// internal/web/templates (issues.templ/issues_templ.go, issuedetail.templ/
// issuedetail_templ.go) раньше был исключён отдельным пунктом: обоснование
// было «рендер бейджа/дропдауна — отдельный шаблонный контур вне охвата».
// Обоснование оказалось неверным — issues.templ уже импортировал
// internal/issue и использовал issue.LevelXxx для уровней в этом же файле,
// то есть техническая граница «шаблоны вне охвата» была пересечена ДО этого
// исключения, просто не для статусов. statusBadgeClass/issueStatusLabel
// (issues.templ) и сравнение it.Status в issuedetail.templ переведены на
// issue.StatusXxx, и оба файла теперь сканируются наравне с остальными
// потребителями — отдельного исключения для этого каталога больше нет.
//
// Полнота набора case/switch в statusBadgeClass/issueStatusLabel (что
// добавление нового значения в issue.Statuses не проходит мимо этих
// функций молча) — не задача этого стража: он ловит копию ЧУЖОГО перечня,
// а не неполноту обработки канона в конкретной функции. Это отдельная
// проверка TestIssueStatusBadgeAndLabelComplete в
// internal/web/templates/helpers_test.go.
//
// Ограничение области импортом issue, а не «весь internal/» — осознанно:
// internal/trace/perfissue.go держит СВОЙ, независимый перечень статусов
// той же формы ("unresolved"/"resolved"/"ignored") для колонки
// perf_issues.status с собственным CHECK-constraint (миграция 0007) — это
// не копия контракта issue, а совпадение словаря у двух независимых сущностей.
// Он не импортирует internal/issue и поэтому сторожем не задет; сканирование
// «любого файла, где встретились эти слова» дало бы по нему ложное
// срабатывание невзирая на то, что это чужой контур, не входящий в задачу.
//
// Механика: внутри каждого просканированного файла ищутся composite-литералы
// (map/slice/массив) и списки case switch, где ВМЕСТЕ, как строковые
// BasicLit одного узла AST, встречаются два и более значения из
// canonStatusLiterals (или canonLevelLiterals). Порог «2 и более в одном
// узле» — это то, что отличает копию ПЕРЕЧНЯ («unresolved»+«resolved»+
// «ignored» вместе, как в старых exportValidStatuses/validStatuses) от
// одиночного случайного попадания слова словаря в код по другой причине
// (например, "error" как ключ структурного лога slog.Error(..., "error",
// err) в internal/ingest/pipeline.go/internal/alert/spike.go — оба
// импортируют issue ради issue.Service, но "error" там встречается по
// одному разу и никогда парой с другим статусом/уровнем в одном узле).
//
// Известные пробелы этой эвристики (честно названы, а не скрыты — сторож
// ловит РЕЦИДИВ известной формы копии, а не класс «любая копия перечня»
// целиком):
//  1. Копия в файле, который НЕ импортирует internal/issue буквальной
//     строкой issueImportPath (например, импортирует его под алиасом, или
//     держит перечень статусов, не имея дела с issue.Service вовсе) —
//     issueEnumCandidateFiles такой файл в кандидаты не возьмёт.
//  2. Копия, разбитая на два (и более) отдельных `if`/`switch` с одним
//     значением в каждом узле, вместо одного узла с двумя и более
//     значениями сразу — classifyLiteralGroup требует ≥2 в ОДНОМ
//     CompositeLit/CaseClause, а не суммарно по файлу.
//  3. Массив/слайс из одного канонического значения плюс отдельная
//     константа/переменная со вторым значением рядом — тот же обход
//     порога «≥2 в одном узле», но с одним значением как литералом, а
//     вторым — как идентификатором вне литерала.
var canonStatusLiterals = map[string]bool{"unresolved": true, "resolved": true, "ignored": true}
var canonLevelLiterals = map[string]bool{"debug": true, "info": true, "warning": true, "error": true, "fatal": true}

// issueImportPath — как импорт internal/issue выглядит в исходнике (с
// кавычками, как в AST ImportSpec.Path.Value).
const issueImportPath = `"gitflic.ru/otezvikentiy/gotcha/internal/issue"`

// issueEnumExcludedDirs — каталоги, исключённые из сканирования целиком (см.
// докблок выше): владелец контракта. internal/web/templates больше не
// исключён — см. докблок пакетного уровня.
var issueEnumExcludedDirs = []string{
	filepath.Join("internal", "issue") + string(filepath.Separator),
}

// issueEnumViolation — одна находка: файл, строка, какой домен (status/
// level) и какие именно литералы встретились вместе.
type issueEnumViolation struct {
	file    string
	line    int
	domain  string
	literal []string
}

// issueEnumCandidateFiles обходит internal/ и возвращает пути (относительно
// root) .go-файлов, которые подлежат сканированию: не тесты, не в
// исключённых каталогах, импортируют internal/issue.
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

// literalGroupLiterals извлекает строковые BasicLit-значения группы узлов
// (Elts составного литерала или List case-клозы switch) — как ключи, так и
// значения KeyValueExpr, и голые элементы.
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

// classifyLiteralGroup сообщает, содержит ли группа литералов 2+ значений
// из canonStatusLiterals или 2+ из canonLevelLiterals — совместно, домен и
// сами найденные значения (обоих доменов сразу не бывает: множества не
// пересекаются лексически, кроме "error", которое есть только в level).
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

// scanFileForIssueEnumCopies разбирает один файл и возвращает найденные
// узлы-нарушители.
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

// TestNoIssueEnumLiteralCopies — основная проверка сторожа: ни один
// потребитель internal/issue не заводит собственный литеральный перечень
// статусов/уровней. См. докблок пакетного уровня выше — область, мутации,
// почему internal/trace/perfissue.go вне охвата.
func TestNoIssueEnumLiteralCopies(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	files := issueEnumCandidateFiles(t, root)
	// Обход ослеп, если кандидатов меньше известного минимума: сегодня их
	// 12 (eventdump.go, web.go, ingest/pipeline.go, ingest/sentry.go,
	// alert/spike.go, export/source_events.go, export/source_issues.go,
	// issuedetail.go, issues.go, exports.go, а также сгенерированные
	// web/templates/issues_templ.go и web/templates/issuedetail_templ.go —
	// с тех пор, как internal/web/templates перестал быть исключением, см.
	// докблок пакетного уровня). Порог ниже фактического — запас на
	// рефакторинг, который уберёт импорт issue из одного-двух файлов не
	// тронув остальные; падение ниже 5 означает, что сканер перестал
	// находить файлы (сломан WalkDir/issueImportPath), а не что
	// потребителей стало меньше.
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

// TestIssueEnumScanExcludesSiblingContract — internal/trace/perfissue.go
// держит СВОЙ независимый перечень статусов той же формы (см. докблок
// пакетного уровня) и не должен попадать в кандидаты сканирования: он не
// импортирует internal/issue. Если бы попал — TestNoIssueEnumLiteralCopies
// падал бы на чужом, законном коде, который эта задача не трогает.
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

// issueEnumStrictFuncs — функции, где ЛЮБОЕ (даже одиночное) вхождение
// строкового литерала из канона статусов/уровней — нарушение, а не только
// группа из двух и более в одном узле AST. Это не ослабление общего
// правила (TestNoIssueEnumLiteralCopies, порог ≥2 в узле), а СУЖЕНИЕ до
// конкретных функций, которые обязаны разбирать весь канон, — точка входа
// находки P1. Порог ≥2 намеренно широкий (см. докблок выше: одиночный
// "error" в slog.Error не должен считаться копией), но именно поэтому он
// не ловит пробел №2 (копия, разбитая по отдельным case-веткам switch —
// ровно так устроены statusBadgeClass/issueStatusLabel: каждый статус в
// своей ветке, а не списком через запятую в одной). Мутационная проверка
// подтвердила это: возврат литерала "resolved" в одну-единственную ветку
// statusBadgeClass не ловится TestNoIssueEnumLiteralCopies вовсе — эта
// проверка нужна как раз для того случая.
var issueEnumStrictFuncs = map[string]bool{
	"statusBadgeClass": true,
	"issueStatusLabel": true,
}

// scanFuncForAnyIssueEnumLiteral разбирает файл и для каждой верхнеуровневой
// функции, чьё имя есть в issueEnumStrictFuncs, ищет ЛЮБОЙ строковый
// BasicLit, совпадающий с canonStatusLiterals/canonLevelLiterals, в её теле
// (в т.ч. одиночный, в отдельной case-ветке switch).
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

// TestIssueStatusBadgeFuncsRejectAnyRawLiteral — точечная проверка ровно
// под находку P1: statusBadgeClass/issueStatusLabel (issues.templ) не
// содержат НИ ОДНОГО литерала канона статусов/уровней, даже одиночного в
// отдельной case-ветке — см. issueEnumStrictFuncs выше про то, почему
// общий TestNoIssueEnumLiteralCopies (порог ≥2 в одном узле) эту форму не
// ловит.
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
