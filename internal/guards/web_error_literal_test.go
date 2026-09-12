package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

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

// без go/types, по имени идентификатора "http" — не ловит алиас или тень пакета.
func isHTTPErrorCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// признаёт только буквальный вызов i18n.T(...), не Tf/Tn, не переменную с
// уже готовой строкой, не результат обёртки — без go/types, по имени "i18n".
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
