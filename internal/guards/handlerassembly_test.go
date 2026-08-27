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

// web.Handler (internal/web/web.go) не собирается одним конструктором:
// New(...) ставит малую часть полей, а cmd/gotcha/main.go доставляет
// остальные вручную, десятками строк вида `webHandler.Поле = значение`.
// Забытая строка не ошибка сборки — Handler компилируется с полем в
// нулевом значении, и раздел молча превращается в 404 (nil-guard срабатывает
// так же, как для сознательно выключенной подсистемы). Ни один существующий
// тест не собирает Handler тем же путём, что main.go: юнит-тесты пакета web
// строят `&Handler{...}` литералом с 1-3 полями, тесты New(...) не
// доводят цепочку присвоений дальше конструктора.
//
// Методика (менять нельзя — на ней разъехались две линзы аудита: без единого
// правила счёта одна насчитала ~45 полей, другая ~30):
//
//  1. reflect.TypeOf(web.Handler{}) даёт ПОЛНЫЙ список полей структуры —
//     экспортируемых и неэкспортируемых разом (reflect отдаёт имена полей
//     независимо от видимости, значения не читаются, unsafe не нужен).
//  2. go/ast разбирает исходный текст (не компилирует, не типизирует —
//     поэтому работает и в состоянии, когда main.go уже не собирается)
//     internal/web/web.go, находит тело New(...) и вынимает ключи
//     композитного литерала `&Handler{...}` — реально устанавливаемые New.
//  3. go/ast тем же приёмом разбирает cmd/gotcha/main.go целиком (не по
//     диапазону строк — они смещаются от правки к правке) и находит КАЖДОЕ
//     присваивание вида `webHandler.Поле = ...`, в любом месте файла и на
//     любой глубине вложенности (в т.ч. `if hostToucher != nil { ... }` —
//     see webHandler.HostForget).
//  4. Поле структуры, не попавшее ни в (2), ни в (3), обязано быть явно
//     перечислено — по ИМЕНИ поля, не по номеру строки и не по счётчику —
//     либо в indirectSetHandlerFields (устанавливается package-internal
//     способом, вызываемым из main.go/server.go, но main.go не может
//     присвоить его напрямую — поле неэкспортируемое), либо в
//     zeroValueHandlerFields (докблок поля прямо говорит "нулевое значение
//     готово к работе" — отсутствие присвоения задокументированное
//     архитектурное решение, а не забывчивость). Всё, что не попало ни в
//     одну из четырёх корзин, — сторож считает забытым полем и падает,
//     называя его по имени.
//  5. Обратное направление: имя, найденное в (2)/(3), которого больше нет
//     среди полей (1), — тоже сигнал (устаревшее присваивание на поле,
//     которого больше нет в структуре). Без AST-разбора это ловит только
//     `go build` — сторож ловит раньше и понятнее: называет поле, а не
//     отдаёт голую ошибку компилятора.
//
// Сторож НЕ трогает 14 файлов internal/web/*_test.go, которые строят
// `&Handler{...}` литералом с 1-3 полями: (2)/(3) разбирают конкретно
// internal/web/web.go (тело New) и cmd/gotcha/main.go — тестовые файлы вне
// поля зрения обоих разборов, а reflect с (1) работает над ТИПОМ, не над
// конкретным значением из теста.
const (
	handlerWebGoFile     = "internal/web/web.go"
	handlerMainGoFile    = "cmd/gotcha/main.go"
	handlerNewFuncName   = "New"
	handlerTypeIdentName = "Handler"
	handlerVarInMain     = "webHandler"
)

// indirectSetHandlerFields — поля, которые New(...) не ставит и main.go не
// может присвоить напрямую (неэкспортируемые — разные пакеты), но которые
// реально заполняются при обычном старте: Handler.Register(mux),
// вызываемый из cmd/gotcha/server.go:62 (deps.webHandler.Register(mux)),
// проставляет их сам (internal/web/web.go:882-883, h.pages/h.routes = ...
// внутри Register). agentLimiter в эту корзину НЕ входит: New(...) уже
// ставит его дефолтом (см. handlerNewFuncName), main.go лишь ПЕРЕЗАПИСЫВАЕТ
// значение через SetAgentDistRateLimit(cfg.AgentDistRatePerMin) — это не
// первичная установка поля, оно и так учтено как "поставлено New".
var indirectSetHandlerFields = map[string]string{
	"pages":  "заполняется внутри Handler.Register(mux) (internal/web/web.go:882), вызываемого из cmd/gotcha/server.go:62 при старте сервера — main.go не может присвоить его напрямую (поле неэкспортируемое)",
	"routes": "заполняется внутри Handler.Register(mux) (internal/web/web.go:883), тем же вызовом, что и pages",
}

// zeroValueHandlerFields — поля, чей докблок в internal/web/web.go прямо
// говорит "нулевое значение готово к работе": отсутствие присвоения —
// сознательное решение автора поля, а не забытая строка. Список
// подтверждён поимённо (см. докблок каждого поля рядом с объявлением),
// добавление нового поля в Handler без явного решения в эту корзину не
// попадёт — сторож потребует либо присвоения, либо записи здесь с причиной.
var zeroValueHandlerFields = map[string]string{
	"agentETags":          "sync.Map — нулевое значение готово к работе (agentdist.go, ленивый ETag-кеш бинарей агента)",
	"ssoProviders":        "ssoCache — нулевое значение готово к работе (sso.go, process-local кеш per-org OIDC-провайдеров)",
	"statusCache":         "statusCache — 30-секундный кеш публичных статус-страниц, нулевое значение готово к работе (statuspage.go)",
	"crossOriginRejected": "atomic.Int64 — нулевое значение готово к работе (crossorigin.go, счётчик отказов same-origin)",
	"coThrottle":          "coThrottle — нулевое значение готово к работе (crossorigin.go, троттлинг лога отказов)",
}

// TestHandlerAssemblyComplete — центральный сторож задачи: каждое поле
// web.Handler обязано быть либо установлено New(...)/main.go, либо
// явно объяснено выше. См. методику в комментарии над indirectSetHandlerFields.
//
// Ограничение: сторож проверяет ФАКТ присвоения (найдено ли где-то в файле
// `webHandler.Поле = ...`), а не его безусловность. `ast.Inspect` заходит на
// любую глубину вложенности, поэтому присвоение внутри `if ... { ... }`
// засчитывается ровно так же, как безусловное на верхнем уровне поля модуля
// — это осознанно (иначе `webHandler.HostForget = hostToucher` внутри
// `if hostToucher != nil { ... }` ложно считался бы забытым), но у этого
// есть цена: НОВОЕ поле, присваиваемое условно и БЕЗ nil-чека на месте
// использования, сторож пропустит молча — то есть ровно тот тихий 404, ради
// которого он написан, для этого конкретного случая не ловится. На момент
// написания условно присваиваемое поле одно — HostForget — и оно защищено с
// обеих сторон: докблок поля документирует nil-safety, а место использования
// (hosts.go:1461) само nil-чекает перед вызовом Forget. Ответственность за
// nil-safety условно присваиваемых полей остаётся на ревьюере задачи,
// добавляющей такое поле, а не на этом сторожe.
func TestHandlerAssemblyComplete(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatalf("поиск корня репозитория: %v", err)
	}

	allFields := handlerFieldNames()
	newFields := parseHandlerNewFields(t, root)
	mainFields := parseHandlerMainAssignFields(t, root)

	// Список исключений сам обязан состоять из реальных полей — иначе
	// переименование поля тихо обесценивает запись (см. мутацию 3 в
	// брифе: список не декоративен).
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

	// Обратное направление (см. методику, пункт 5): присвоение на поле,
	// которого в структуре уже нет.
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

// handlerFieldNames — имена ВСЕХ полей web.Handler (экспортируемых и нет):
// reflect отдаёт имя поля по позиции независимо от видимости, значения не
// читаются — unsafe не нужен ни здесь, ни где-либо ниже.
func handlerFieldNames() map[string]bool {
	typ := reflect.TypeOf(web.Handler{})
	set := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		set[typ.Field(i).Name] = true
	}
	return set
}

// parseHandlerNewFields разбирает internal/web/web.go, находит тело
// функции New(...) (без receiver) и вынимает ключи композитного литерала
// `&Handler{...}` — поля, которые New реально устанавливает. ast.Inspect,
// а не прямой разбор ReturnStmt: защищает от будущего рефакторинга New
// (например, промежуточная переменная `h := &Handler{...}; return h`) без
// необходимости переписывать сторож.
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

// parseHandlerMainAssignFields разбирает cmd/gotcha/main.go ЦЕЛИКОМ (не по
// диапазону строк) и находит каждое присваивание вида
// `webHandler.Поле = значение` через ast.Inspect — тот же обход находит
// присваивание и внутри вложенных блоков (`if hostToucher != nil { ... }`,
// webHandler.HostForget), не только на верхнем уровне. Методом вызова
// (`webHandler.SetAgentDistRateLimit(...)`) и чтением через селектор
// (`webHandler.CrossOriginRejected` как аргумент-функция) не считаются —
// это не *ast.AssignStmt, а *ast.ExprStmt/аргумент вызова.
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
