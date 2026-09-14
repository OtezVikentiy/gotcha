package guards

import (
	"regexp"
	"strings"
	"testing"
)

// lvlPublic и lvlUser не требуют своего гейта в теле хендлера (lvlUser уже
// даёт requireUser при регистрации маршрута) — остальные требуют.
var authzGateRequiredLevels = map[string]bool{
	"lvlAccess":        true,
	"lvlOperator":      true,
	"lvlAdmin":         true,
	"lvlOwner":         true,
	"lvlInstanceAdmin": true,
}

var routeAuthzEntryRe = regexp.MustCompile(`"((?:GET|POST) [^"]+)":\s*(lvl\w+),`)

func routeAuthzLevels(t *testing.T, tree *Tree) map[string]string {
	t.Helper()
	for _, f := range tree.GoFiles {
		if f.Path != "internal/web/authz_map_test.go" {
			continue
		}
		out := map[string]string{}
		for _, m := range routeAuthzEntryRe.FindAllStringSubmatch(f.Body, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	t.Fatalf("не нашли internal/web/authz_map_test.go в дереве")
	return nil
}

// requireUser заворачивает КАЖДЫЙ маршрут выше lvlPublic одинаково — резолвит
// хендлер, а не уровень; что реально проверяется в его теле — отдельный шаг.
var requireUserRouteRe = regexp.MustCompile(`inner\.Handle\("((?:GET|POST) [^"]+)",\s*h\.requireUser\(http\.HandlerFunc\(h\.(\w+)\)\)\)`)

func routeHandlers(t *testing.T, tree *Tree) map[string]string {
	t.Helper()
	for _, f := range tree.GoFiles {
		if f.Path != "internal/web/web.go" {
			continue
		}
		out := map[string]string{}
		for _, m := range requireUserRouteRe.FindAllStringSubmatch(f.Body, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	t.Fatalf("не нашли internal/web/web.go в дереве")
	return nil
}

// Список закрыт намеренно: новый способ гейтить маршрут обязан попасть в один
// из уже перечисленных вызовов, а не завести свой.
var projectOrOrgGateCalls = []string{
	"requireProjectOperator(",
	"requireProjectRole(",
	"requireProjectOwner(",
	"requireOrgRole(",
	"requireOrgOwner(",
	"requireTeamRole(",
	"requireInstanceAdminForSSO(",
	"CanAccessProject(",
}

// Матчит h.Xxx(...) — по нему собираются вызовы РОВНО одного шага делегирования.
var handlerCallRe = regexp.MustCompile(`\bh\.(\w+)\(`)

func handlerBodyLines(tree *Tree, handlerName string) (lines []string, exists bool) {
	target := "Handler." + handlerName
	for _, f := range tree.GoFiles {
		if f.Generated || !strings.HasPrefix(f.Path, "internal/web/") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		ctx := funcContexts(f.Body)
		all := strings.Split(f.Body, "\n")
		for i, c := range ctx {
			if c == target {
				exists = true
				lines = append(lines, all[i])
			}
		}
	}
	return lines, exists
}

func linesContainGate(lines []string) bool {
	for _, line := range lines {
		for _, gate := range projectOrOrgGateCalls {
			if strings.Contains(line, gate) {
				return true
			}
		}
	}
	return false
}

// Ищет гейт в теле самого хендлера или в теле одной вызванной им функции —
// глубже одного шага делегирования не смотрит (текстовый скан, не граф вызовов).
func authzHandlerHasGate(tree *Tree, handlerName string) (hasGate, exists bool) {
	lines, exists := handlerBodyLines(tree, handlerName)
	if !exists {
		return false, false
	}
	if linesContainGate(lines) {
		return true, true
	}
	seenCallee := map[string]bool{}
	for _, line := range lines {
		for _, m := range handlerCallRe.FindAllStringSubmatch(line, -1) {
			seenCallee[m[1]] = true
		}
	}
	for callee := range seenCallee {
		calleeLines, calleeExists := handlerBodyLines(tree, callee)
		if calleeExists && linesContainGate(calleeLines) {
			return true, true
		}
	}
	return false, true
}

// Ниже факта (117 маршрутов требуют гейта из 137 резолвленных) — падение
// сигналит, что регэкспы разошлись с web.go/authz_map_test.go, а не что маршруты исчезли.
const minAuthzRoutesResolved = 100
const minAuthzGateChecked = 110

// Хендлер (или вызванная им напрямую функция) зовёт хотя бы один гейт.
// Соответствие силы гейта уровню НЕ проверяется — это поведенческая проверка.
func TestAuthzLevelHandlersCallSomeProjectOrOrgGate(t *testing.T) {
	tree := Load(t)
	levels := routeAuthzLevels(t, tree)
	handlers := routeHandlers(t, tree)

	if len(levels) < minAuthzRoutesResolved {
		t.Fatalf("routeAuthz дал %d маршрутов, ожидалось не меньше %d — разбор authz_map_test.go разошёлся с файлом", len(levels), minAuthzRoutesResolved)
	}
	if len(handlers) < minAuthzRoutesResolved {
		t.Fatalf("резолвер web.go дал %d маршрутов, ожидалось не меньше %d — разбор web.go разошёлся с файлом", len(handlers), minAuthzRoutesResolved)
	}

	checked := 0
	for route, lvl := range levels {
		if !authzGateRequiredLevels[lvl] {
			continue
		}
		handlerName, ok := handlers[route]
		if !ok {
			// Три lvlPublic-маршрута регистрируются не через requireUser-обёртку — пропускаем.
			continue
		}
		hasGate, exists := authzHandlerHasGate(tree, handlerName)
		if !exists {
			t.Errorf("%s (%s): хендлер %s не найден в internal/web/*.go — резолвер разошёлся с кодом", route, lvl, handlerName)
			continue
		}
		checked++
		if !hasGate {
			t.Errorf("%s: уровень %s объявлен в routeAuthz, но ни хендлер %s, ни вызванная им функция не зовут ни один из %v — гейт отсутствует",
				route, lvl, handlerName, projectOrOrgGateCalls)
		}
	}
	if checked < minAuthzGateChecked {
		t.Fatalf("проверено %d маршрутов access/operator/admin/owner/instance_admin, ожидалось не меньше %d — резолвер маршрутов ослеп", checked, minAuthzGateChecked)
	}
}
