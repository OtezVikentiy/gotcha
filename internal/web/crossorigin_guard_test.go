package web_test

import (
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/guards"
)

// TestMutatingRoutesRequireOrigin промолчит при частичном (не полном) ослеплении
// RegisteredRoutes() — ниже факта (85 не исключённых POST-маршрутов сегодня).
const minTestablePostRoutes = 80

func TestMutatingRoutesCoverageDoesNotGoBlind(t *testing.T) {
	s := newUptimeStack(t)
	exempt := guards.ExemptedValues(originExemptions)

	tested := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, _, ok := strings.Cut(route, " ")
		if !ok || method != http.MethodPost {
			continue
		}
		if exempt[route] {
			continue
		}
		tested++
	}
	if tested < minTestablePostRoutes {
		t.Fatalf("проверяемых (не исключённых) POST-маршрутов %d, ожидалось не меньше %d — "+
			"обход RegisteredRoutes() ослеп частично, TestMutatingRoutesRequireOrigin такое не поймает",
			tested, minTestablePostRoutes)
	}
}
