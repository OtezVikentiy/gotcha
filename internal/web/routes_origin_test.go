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

// originExemptions — мутирующие маршруты, которым Origin не присылают по
// построению: это машинные ручки, а не запросы браузера, и требовать от них
// Origin/Referer означало бы отклонять легитимный machine-to-machine трафик
// (heartbeat-пинг из cron, приём результатов выносной пробы). Находка №18
// аудита (QA-6), план сторожей, задача 10.
//
// Максимум зафиксирован в 3 — ровно столько машинных мутирующих маршрутов
// сейчас существует (см. блок `if h.Uptime != nil` в Register, web.go). Новый
// машинный маршрут — осознанно поднять потолок; пропавший — CheckExemptions
// провалит сборку сам (устаревшее исключение).
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

// concretePath подставляет в шаблон маршрута ("/teams/{id}/rename") валидный,
// но заведомо чужой идентификатор вместо {id}/{token}/{transaction...}.
// Проверке Origin, которая по всему пакету стоит первой строкой в
// обработчике (до auth.UserID, до разбора пути и до requireOrgRole/
// requireProjectRole), неважно, существует ли ресурс за идентификатором —
// поэтому дурного значения "1" достаточно, реальные org/project/monitor не
// нужны.
func concretePath(path string) string {
	return routePlaceholder.ReplaceAllString(path, "1")
}

// TestMutatingRoutesRequireOrigin — перебор ВСЕХ POST-маршрутов из
// h.RegisteredRoutes(): запрос с валидной сессией и без заголовка Origin
// обязан получить 403.
//
// До этой задачи проверка «нет Origin → 403» перечисляла маршруты вручную
// (TestCoverSameOriginGuards, cover_cheap_test.go) — список отстал от
// регистрации и оставил без покрытия восемь мутирующих маршрутов (находка
// №18 аудита, QA-6): отзыв приглашения, изменение канала оповещений,
// изменение окна обслуживания, перевыпуск heartbeat-токена, переименование
// команды, отзыв пробы, создание проекта, выход. К моменту исполнения этой
// задачи все восемь уже несли sameOrigin в самих обработчиках (детали и
// построчные ссылки — в task-10-report.md): сторож это подтверждает и не
// даёt регрессии повториться незамеченной, а не чинит несуществующие дыры.
//
// Стенд — newUptimeStack (heartbeat_test.go), а не newStack: только на нём
// h.Uptime собран и регистрируются все три машинных маршрута из
// originExemptions (Register гейтит их за `if h.Uptime != nil`) — без этого
// они не попали бы в RegisteredRoutes() и CheckExemptions ошибочно счёл бы
// исключения устаревшими.
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
