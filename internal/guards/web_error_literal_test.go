package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// machineResponseFiles — файлы internal/web, чьи http.Error(...) отвечают не
// браузеру человека, а программе (install-скрипту, воркеру), которая тело
// ответа не рендерит и локаль не спрашивает — переводить его на i18n
// незачем. Список ведётся ЯВНО: добавление файла сюда — осознанное решение
// ревью задачи, а не эвристика по пути/имени функции, которая незаметно
// размывается со временем (T5, волна 3 аудита 2026-09-05, K7-10 — тогда же
// заведён этот сторож).
var machineResponseFiles = map[string]string{
	"internal/web/agentdist.go": "installSh отдаёт install.sh для curl | sh, agentFile — " +
		"бинарь агента напрямую: оба потребителя не рендерят HTML и не смотрят на " +
		"Accept-Language браузера",
	"internal/web/probeapi.go": "API для внешних uptime-проб (лизинг заданий, приём " +
		"результатов) — потребитель probe-раннер, а не браузер; ответы уже JSON через " +
		"writeProbeError, i18n здесь бессмыслен",
	"internal/web/heartbeat.go": "приём heartbeat-пинга от агента/cron — потребитель " +
		"скрипт, а не браузер; ответы уже JSON через writeHeartbeatJSON, i18n здесь " +
		"бессмыслен",
}

// TestNoLiteralHTTPErrorInWeb — находка K7-10: около 86 мест в internal/web
// отвечали http.Error с английским литералом мимо i18n, пользователь с
// русской локалью получал на месте страницы ошибки английский текст.
// Человеческий ответ обязан идти через h.renderError с ключом i18n (см.
// denyCrossOrigin/renderError в web.go) — ключ заводится в ОБЕИХ локалях
// (internal/i18n/locales/ru.json и en.json).
//
// Разбор — go/ast, а не регэксп по тексту строки (фикс-раунд 1 этой же
// задачи держался на регэкспе `http\.Error\(\s*\w+\s*,\s*"`, который видел
// только литерал СРАЗУ вторым аргументом; ревью фикс-раунда 2 обошло его
// тривиально — вынесло тот же литерал в переменную,
// `mutMsg := "…"; http.Error(w, mutMsg, …)`, и регэксп промолчал). AST не
// разбирает, ЧТО ИМЕННО стоит вторым аргументом текстом — он смотрит на
// структуру выражения: правило "второй аргумент http.Error — вызов
// i18n.T(...)" истинно или ложно независимо от того, как автор попытался
// его замаскировать (переменная, fmt.Sprintf, конкатенация, отдельная
// вспомогательная функция, которая сама что-то возвращает не через i18n).
// ast.Inspect обходит ВЕСЬ файл рекурсивно, включая замыкания и вложенные
// функции — вызов, спрятанный внутри анонимной функции внутри обработчика,
// найдётся так же, как и вызов на верхнем уровне метода.
//
// Единственный сегодня законный вызов http.Error в человеческом файле —
// logs.go:274 (`http.Error(w, i18n.T(r.Context(), "error.internal"),
// http.StatusInternalServerError)`, JSON-эндпоинт автокомплита логов) —
// и он ровно демонстрирует форму, которую сторож требует: второй аргумент —
// вызов i18n.T(...), не литерал, не переменная, не результат другой обёртки.
//
// Упадёт так: новый http.Error(w, X, ...) в человеческом файле, где X — не
// вызов i18n.T(...) — перевести на h.renderError(w, r, status,
// i18n.T(r.Context(), "error…")) (или, если человеческий ответ ДОЛЖЕН
// остаться голым http.Error по образцу logs.go, обернуть текст в
// i18n.T(...) на месте). Если ответ на самом деле машинный (не для браузера
// человека, тело не рендерится как страница) — внести файл в
// machineResponseFiles с обоснованием, а не менять этот тест.
func TestNoLiteralHTTPErrorInWeb(t *testing.T) {
	tree := Load(t)
	fset := token.NewFileSet()
	for _, f := range tree.GoFiles {
		if !strings.HasPrefix(f.Path, "internal/web/") || f.Generated ||
			strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		if _, exempt := machineResponseFiles[f.Path]; exempt {
			continue
		}
		parsed, err := parser.ParseFile(fset, f.Path, f.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f.Path, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isHTTPErrorCall(call) || len(call.Args) < 2 {
				return true
			}
			if isI18nTCall(call.Args[1]) {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			t.Errorf("%s: http.Error, второй аргумент которого — не вызов i18n.T(...), — "+
				"мимо i18n, пользователь с русской локалью увидит английский текст (или "+
				"наоборот). Переведите на h.renderError(w, r, status, i18n.T(r.Context(), "+
				"\"error.…\")) с ключом в обеих локалях, либо, если ответ машинный (не для "+
				"браузера человека), внесите файл в machineResponseFiles с обоснованием",
				pos)
			return true
		})
	}
}

// isHTTPErrorCall — true для вызова вида http.Error(...): Fun — селектор
// с пакетным идентификатором "http" и именем "Error". Так же наивно, как и
// остальные разборы этого пакета (см. докблок collectSelfMetrics в
// selfmetrics_names_test.go) — без go/types, по имени идентификатора; в
// этом дереве net/http нигде не импортируется под алиасом и не шэдовится
// локальной переменной "http", проверено по всему internal/web.
func isHTTPErrorCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// isI18nTCall — true, если arg — это вызов i18n.T(...) буквально (не
// i18n.Tf/i18n.Tn, не переменная, в которую он сохранён, не результат
// обёртки над ним): единственная форма, которую сегодня демонстрирует
// законный случай logs.go:274. Уже вызванный T() результат — обычная
// string, и рекурсивно проверить, откуда она взялась дальше по цепочке
// присваиваний, AST на уровне одного выражения не может — оставлять здесь
// более широкое правило означало бы гадать, а не проверять (тот же принцип,
// что и в докблоке ContentAnchor про то, чего схема не гарантирует).
func isI18nTCall(arg ast.Expr) bool {
	call, ok := arg.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "i18n"
}
