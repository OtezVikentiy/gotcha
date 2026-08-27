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

// Сторож на pageComponents() (internal/web/templates/errorpaths_test.go):
// карта конструкторов, на которой держатся ОБЕ инвариантные проверки
// шаблонов — TestRenderPropagatesWriteErrors (шаблон не глотает ошибку
// writer'а) и TestRenderRespectsCancelledContext (отменённый контекст
// возвращает ошибку рендера), — не должна тихо не знать о новой странице.
//
// Так уже случилось: 17 из 59 страничных компонентов (About, Exports,
// LogsScreen, HostsList, Escalations, оба экрана SLO, IncidentFeed и
// другие — вся молодая половина продукта) не входили в карту, и оба
// инварианта на них не проверялись вовсе, хотя выглядели проверенными по
// всему дереву (находка волны 2 полного аудита, кластер 8/10 DEDUP-P1.md).
//
// Истина — СГЕНЕРИРОВАННЫЕ _templ.go пакета templates, а не второй
// рукописный список: второй рукописный список расходится с первым так же
// молча, как единственный список расходится с кодом (тот же класс, что и
// autoBufferCapUnits в buffer_units_test.go и projectTables в
// ch_tables_test.go). _templ.go — обычный Go, разбираемый go/parser без
// компромиссов; он лишь дублирует свой .templ-источник (тот же приём
// исключения, каким tree.go размечает Generated), поэтому парсить именно
// его, а не писать регулярку по нестандартному синтаксису .templ, —
// не подмена источника истины, а выбор ФОРМЫ того же источника.
//
// Страничный компонент — экспортированная функция ВЕРХНЕГО уровня (без
// получателя) с единственным результатом templ.Component. Проверено по
// факту на дереве на момент написания сторожа: этому определению отвечают
// ровно 59 функций, и ни одна из них не является более мелким переиспользуемым
// виджетом (icons.templ/modal.templ/help.templ/emptystate.templ/banner.templ
// и подобные держат свои templ-блоки НЕэкспортированными как раз для того,
// чтобы это определение не задело их).
const (
	templatesDir         = "internal/web/templates"
	pageComponentsFile   = "internal/web/templates/errorpaths_test.go"
	pageComponentsFunc   = "pageComponents"
	templComponentPkg    = "templ"
	templComponentTypeID = "Component"
)

// pageComponentExceptions — страничные компоненты, сознательно не входящие в
// pageComponents(), и почему (см. брифа задачи). Пусто на сегодня: у всех 59
// конструктор собирается из экспортированных типов без сторонних
// зависимостей, исключений не потребовалось — там, где фикстура выглядела
// нетривиальной (HostDetail, IncidentFeed, LogsScreen), она всё равно
// честно строится из реальных типов пакетов host/incidentgroup/log.
//
// wantExceptions ниже — ПИН на размер этой карты, тем же приёмом, что
// TestBufferShareConstantsPinned в buffer_units_test.go: молчаливое
// исключение новой страницы (не завели фикстуру, воспользовались обходным
// путём) поднимет len() этой карты и уронит сторож ниже с требованием
// обосновать новую строку явно, а не просто дописать её.
var pageComponentExceptions = map[string]string{}

const wantExceptions = 0

func TestPageComponentsMapComplete(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	declared := scanDeclaredPageComponents(t, root)
	// Нижняя граница — только на то, что даёт обход _templ.go: обход,
	// нашедший меньше, сломан сам (сузился до одного файла, перестал видеть
	// экспортированные функции), а без этой проверки пустой результат
	// совпал бы с пустой картой исключений и тест был бы зелёным вникуда.
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

// scanDeclaredPageComponents обходит СГЕНЕРИРОВАННЫЕ *_templ.go пакета
// templates и возвращает множество имён экспортированных функций верхнего
// уровня (без получателя), возвращающих ровно один результат templ.Component.
// Пустой каталог/файл без единой такой функции — ослепший сторож (у пакета
// templates не может не быть страниц), поэтому нижняя граница проверяется
// вызывающим (TestPageComponentsMapComplete), а не здесь: здесь функция
// только собирает то, что нашла.
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

// returnsTemplComponent — сигнатура вида func(...) templ.Component, ровно
// один результат. Компилятор templ генерирует именно такую сигнатуру для
// каждого блока `templ Name(...) { ... }`, других способов получить функцию
// с таким результатом в *_templ.go нет.
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

// scanUsedPageComponents разбирает pageComponentsFile, находит тело функции
// pageComponentsFunc и возвращает множество имён конструкторов, вызванных
// как ЗНАЧЕНИЯ записей map[string]... с СТРОКОВЫМ ключом — то есть записей
// вида `"Ключ": Конструктор(...)` внутри карты pageComponents(). Это
// единственная композитная форма в теле функции, где значение — прямой
// вызов пакетного идентификатора: точечная привязка к форме карты, а не
// сбор ВСЕХ идентификаторов функции (тот сбор попал бы на int64(...) внутри
// вложенного `func() *int64 { ... }()` и на другие служебные вызовы фикстур,
// не имеющие отношения к страничным компонентам).
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
