package web_test

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/guards"
)

// Машинные ручки — cron/выносная проба, не браузер, Origin/Referer не несут по построению.
// Максимум 3 — новый машинный маршрут поднимает потолок осознанно; пропавший роняет сборку.
var originExemptions = []guards.Exemption{
	{
		Value:   "POST /uptime/hb/{token}",
		Why:     "публичный heartbeat-пинг из cron/скрипта — не браузер, Origin/Referer не несёт по построению",
		Finding: "№18/QA-6",
	},
	{
		Value:   "POST /probe/lease",
		Why:     "lease-протокол выносной пробы: аутентификация Bearer-токеном, не сессией браузера",
		Finding: "№18/QA-6",
	},
	{
		Value:   "POST /probe/results",
		Why:     "приём результатов выносной пробы — тот же машинный API, что и /probe/lease",
		Finding: "№18/QA-6",
	},
}

var routePlaceholder = regexp.MustCompile(`\{[^}]*\}`)

// Origin-проверка стоит первой в обработчике, до auth/разбора пути и existence-oracle — ей
// неважно, существует ли ресурс, так что "1" вместо {id} достаточно.
func concretePath(path string) string {
	return routePlaceholder.ReplaceAllString(path, "1")
}

// Стенд — newUptimeStack, не newStack: только на нём h.Uptime собран и регистрируются все
// три машинных маршрута из originExemptions — иначе CheckExemptions счёл бы их устаревшими.
func TestMutatingRoutesRequireOrigin(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "origin-guard@example.com")

	exempt := guards.ExemptedValues(originExemptions)
	seen := make(map[string]bool)

	tested := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, path, ok := strings.Cut(route, " ")
		if !ok || method != http.MethodPost {
			continue
		}
		seen[route] = true
		if exempt[route] {
			continue
		}
		tested++

		concrete := concretePath(path)
		resp := postForm(t, s.srv, concrete, url.Values{}, "", cookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s (без Origin) статус = %d, ожидали 403", method, concrete, resp.StatusCode)
		}
	}

	if tested == 0 {
		t.Fatal("не найдено ни одного проверяемого POST-маршрута — RegisteredRoutes() пуст?")
	}

	guards.CheckExemptions(t, "TestMutatingRoutesRequireOrigin", originExemptions, 3, seen)
}
