package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/guards"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// /static/ отдаёт статические ассеты через cacheControl (web.go:308), который своим
// Cache-Control перекрывает no-store из securityHeaders ниже по цепочке — версионированные
// файлы обязаны кэшироваться, ПДн там нет.
var cacheControlExemptions = []guards.Exemption{
	{
		Value:   "GET /static/",
		Why:     "версионированная статика: cacheControl (web.go:308) намеренно ставит max-age вместо no-store",
		Finding: "T1",
	},
}

// Регресс 1.4.0: ни один тест слоя сжатия и ни один тест страниц не смотрел на
// исходящий Content-Type — httptest.NewRecorder не сериализует ответ, досниффинг
// не воспроизводится. Сторож ходит через настоящий httptest.NewServer.
func TestResponseHeadersOnEveryGETRoute(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "headers-guard@example.com")

	client := noRedirectClient()
	cacheExempt := guards.ExemptedValues(cacheControlExemptions)
	seenCache := make(map[string]bool)

	tested := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, path, ok := strings.Cut(route, " ")
		if !ok || method != http.MethodGet {
			continue
		}
		tested++

		concrete := concretePath(path)
		req, err := http.NewRequest(http.MethodGet, s.srv.URL+concrete, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", route, err)
		}
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: do request: %v", route, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, ожидался nosniff", route, got)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, ожидался DENY", route, got)
		}
		if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy = %q, ожидался same-origin", route, got)
		}
		seenCache[route] = true
		if got := resp.Header.Get("Cache-Control"); got != "no-store" && !cacheExempt[route] {
			t.Errorf("%s: Cache-Control = %q, ожидался no-store", route, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s: Content-Security-Policy пуст", route)
		}
		if ct := resp.Header.Get("Content-Type"); ct == "" {
			t.Errorf("%s: Content-Type не объявлен — тип будет досниффен получателем", route)
		}
		if resp.StatusCode >= 500 {
			t.Errorf("%s: статус %d на синтетическом идентификаторе — дефект обработчика, а не особенность проверки", route, resp.StatusCode)
		}
	}

	if tested == 0 {
		t.Fatal("не найдено ни одного проверяемого GET-маршрута — RegisteredRoutes() пуст?")
	}
	guards.CheckExemptions(t, "TestResponseHeadersOnEveryGETRoute", cacheControlExemptions, 2, seenCache)
}

// Уровень 2: критерий — не статус, а сам факт «HTML тяжелее килобайта прошёл через
// gzipSSR». Регрессу 1.4.0 было всё равно, что за страница — он ломал общий слой, а не
// конкретный хендлер, поэтому фикстуры по каждому домену не нужны: синтетический
// идентификатор (как в TestResponseHeadersOnEveryGETRoute) достаточен — стилизованная 404
// и пустое состояние списка сами набирают килобайт через общий шаблон шапки/навигации
// и проходят тот же gzipSSR, что и страница с данными.
func TestGETRoutesGzipContentTypeParity(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	_, cookie := orgSettingsRegister(t, authSvc, "gzip-parity@example.com")

	tested := 0
	htmlOver1KB := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, path, ok := strings.Cut(route, " ")
		if !ok || method != http.MethodGet {
			continue
		}
		tested++
		concrete := concretePath(path)

		plain := fetchSSR(t, s.srv, concrete, cookie, false)
		plainBody, err := io.ReadAll(plain.Body)
		plain.Body.Close()
		if err != nil {
			t.Fatalf("%s: read plain body: %v", route, err)
		}
		plainCT := plain.Header.Get("Content-Type")
		if !strings.HasPrefix(plainCT, "text/html") || len(plainBody) <= 1024 {
			continue
		}
		htmlOver1KB++

		gz := fetchSSR(t, s.srv, concrete, cookie, true)
		io.Copy(io.Discard, gz.Body)
		gz.Body.Close()
		if enc := gz.Header.Get("Content-Encoding"); enc != "gzip" {
			t.Errorf("%s: тело без сжатия %d байт, Content-Type %q, но Content-Encoding с gzip = %q, ожидался gzip",
				route, len(plainBody), plainCT, enc)
		}
		if ct := gz.Header.Get("Content-Type"); ct != plainCT {
			t.Errorf("%s: Content-Type с gzip = %q, без gzip = %q — разъехались", route, ct, plainCT)
		}
	}

	if tested == 0 {
		t.Fatal("не найдено ни одного проверяемого GET-маршрута — RegisteredRoutes() пуст?")
	}
	// Снято прогоном на момент написания (68 GET-маршрутов, 64 дали text/html тяжелее
	// килобайта на синтетическом идентификаторе) — падение ниже значит, что часть
	// маршрутов перестала доезжать до проверки (схлопнулась в редирект, стала короче
	// килобайта или потеряла text/html), а не что регресса больше нет.
	const wantHTMLOver1KB = 64
	if htmlOver1KB < wantHTMLOver1KB {
		t.Errorf("HTML-ответов тяжелее 1024 байт = %d, ожидалось не меньше %d — часть маршрутов выпала из проверки", htmlOver1KB, wantHTMLOver1KB)
	}
}

// Страницы, обязанные отдать именно 200 — страховка от вырождения счётчика выше: если
// всё дерево начнёт отвечать 404, wantHTMLOver1KB может формально сойтись на одних
// стилизованных ошибках, а эти 5 конкретных страниц обязаны рендериться с данными.
var ssrPagesMustRender = []string{
	"GET /{$}",
	"GET /login",
	"GET /projects/{id}/issues",
	"GET /monitors/{id}",
	"GET /orgs/{id}/settings",
}

func rawClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func fetchSSR(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie, gzip bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("%s: new request: %v", path, err)
	}
	req.AddCookie(cookie)
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatalf("%s: do request: %v", path, err)
	}
	return resp
}

// Мутация регресса 1.4.0: убрать w.Header().Set("Content-Type", ct) в ssrgzip.go — обязана
// покраснеть здесь, на реальной странице из списка выше, а не только в модульном тесте слоя.
func TestSSRPagesKeepHTMLTypeWhenCompressed(t *testing.T) {
	s := newUptimeStack(t)
	// monitorDetail 404-ит без UptimeQuery (monitors.go:248) — newUptimeStack его не
	// заводит, chOnce переиспользует уже поднятый контейнер, второе соединение дёшево.
	s.h.UptimeQuery = uptime.NewQuery(testenv.MigratedCH(t))
	authSvc := auth.NewService(s.pool)
	uid, cookie := orgSettingsRegister(t, authSvc, "ssr-headers@example.com")

	o, err := s.h.Org.CreateOrg(context.Background(), "ssr-headers-co", "SSR Headers Co", uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.h.Org.CreateProject(context.Background(), o.ID, "ssr-headers-proj", "SSR Headers Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// GET / со своим проектом только редиректит (web.go:670) — 200 отдаёт лишь члену
	// организации без единого доступного проекта (web.go:649-665), поэтому под него
	// отдельный аккаунт.
	rootUID, rootCookie := orgSettingsRegister(t, authSvc, "ssr-headers-root@example.com")
	if _, err := s.h.Org.CreateOrg(context.Background(), "ssr-headers-root-co", "SSR Headers Root Co", rootUID); err != nil {
		t.Fatalf("create root org: %v", err)
	}

	hbConfig, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID:         proj.ID,
		Name:              "SSR headers monitor",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            hbConfig,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	pages := map[string]string{
		"GET /{$}":                  "/",
		"GET /login":                "/login",
		"GET /projects/{id}/issues": fmt.Sprintf("/projects/%d/issues", proj.ID),
		"GET /monitors/{id}":        fmt.Sprintf("/monitors/%d", created.ID),
		"GET /orgs/{id}/settings":   fmt.Sprintf("/orgs/%d/settings", o.ID),
	}

	for _, route := range ssrPagesMustRender {
		path, ok := pages[route]
		if !ok {
			t.Fatalf("%s: путь не задан в фикстуре теста", route)
		}
		reqCookie := cookie
		if route == "GET /{$}" {
			reqCookie = rootCookie
		}

		plain := fetchSSR(t, s.srv, path, reqCookie, false)
		plainBody, err := io.ReadAll(plain.Body)
		plain.Body.Close()
		if err != nil {
			t.Fatalf("%s: read plain body: %v", route, err)
		}
		if plain.StatusCode != http.StatusOK {
			t.Errorf("%s: статус без gzip = %d, ожидался 200", route, plain.StatusCode)
		}
		if len(plainBody) <= 1024 {
			t.Errorf("%s: тело без сжатия = %d байт, ожидалось больше 1024", route, len(plainBody))
		}
		if ct := plain.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type без gzip = %q, ожидался text/html", route, ct)
		}

		gz := fetchSSR(t, s.srv, path, reqCookie, true)
		io.Copy(io.Discard, gz.Body)
		gz.Body.Close()
		if gz.StatusCode != http.StatusOK {
			t.Errorf("%s: статус с gzip = %d, ожидался 200", route, gz.StatusCode)
		}
		if enc := gz.Header.Get("Content-Encoding"); enc != "gzip" {
			t.Errorf("%s: Content-Encoding = %q, ожидался gzip", route, enc)
		}
		if ct := gz.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type с gzip = %q, ожидался text/html", route, ct)
		}
	}
}

// POST-маршруты, где отказ формы не редиректит, а рендерит всю страницу инлайн с 422 —
// тот же gzipSSR, что и у уровня 2, но на пути POST. Пустая форма достаточна для 422
// на всех трёх: пустой email не проходит validInviteEmail, spike-правило с нулевыми
// threshold/window не проходит validateRule, пустой edge не проходит validateShape.
var inlineErrorPagesMustRender = map[string]string{
	"POST /orgs/{id}/settings/invite":       "",
	"POST /projects/{id}/alerts/rules":      "",
	"POST /projects/{id}/alert-suppression": "",
}

func TestInlineErrorPagesKeepHTMLType(t *testing.T) {
	s := newUptimeStack(t)
	s.h.Alerts = alert.NewService(s.pool)
	s.h.AlertDeps = depsuppress.NewStore(s.pool)
	authSvc := auth.NewService(s.pool)
	uid, cookie := orgSettingsRegister(t, authSvc, "inline-headers@example.com")

	o, err := s.h.Org.CreateOrg(context.Background(), "inline-headers-co", "Inline Headers Co", uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.h.Org.CreateProject(context.Background(), o.ID, "inline-headers-proj", "Inline Headers Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	paths := map[string]string{
		"POST /orgs/{id}/settings/invite":       "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/invite",
		"POST /projects/{id}/alerts/rules":      "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts/rules",
		"POST /projects/{id}/alert-suppression": "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression",
	}

	for route := range inlineErrorPagesMustRender {
		path, ok := paths[route]
		if !ok {
			t.Fatalf("%s: путь не задан в фикстуре теста", route)
		}

		resp := postForm(t, s.srv, path, url.Values{}, s.srv.URL, cookie)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read body: %v", route, err)
		}
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: статус = %d, ожидался 422: %s", route, resp.StatusCode, body)
		}
		if len(body) <= 1024 {
			t.Errorf("%s: тело = %d байт, ожидалось больше 1024", route, len(body))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, ожидался text/html", route, ct)
		}
	}
}

// Полнота POST-стороны закрывается не динамическим перебором (среди 92 POST-маршрутов
// есть удаление проекта, отзыв ключей и т.п. — пустая форма на них разрушила бы фикстуру
// теста), а статической сверкой: счётчик мест инлайн-рендера с 422 в исходниках пакета.
// Новый такой рендер меняет число, тест падает, автор либо покрывает его динамически
// выше, либо осознанно поднимает wantInlineUnprocessableEntity.
func TestInlineUnprocessableEntityCountMatchesSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	total := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		total += strings.Count(string(data), "StatusUnprocessableEntity")
	}

	// Снято прогоном на момент написания (grep -rn StatusUnprocessableEntity internal/web/*.go,
	// без _test.go): 97 вхождений в 21 файле.
	const wantInlineUnprocessableEntity = 97
	if total != wantInlineUnprocessableEntity {
		t.Errorf("StatusUnprocessableEntity встречается %d раз в internal/web/*.go (без тестов), ожидалось %d — "+
			"новый инлайн-рендер 422 не учтён сторожем: покройте маршрут динамически в TestInlineErrorPagesKeepHTMLType "+
			"либо осознанно поднимите wantInlineUnprocessableEntity", total, wantInlineUnprocessableEntity)
	}
}
