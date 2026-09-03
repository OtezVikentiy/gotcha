package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// wireRecipes заводит на стенде RuleService порогов — страницы рецептов
// считают по нему статусы «создан/будет создан», а POST создаёт правила.
// h.Metrics (ClickHouse) сознательно НЕ проводится: путь «данные приходят»
// требует живого CH и проверяется в T5, здесь везде ветка «ждём данные»
// (пер-задачному ревью T4 это не пробел, а границa задачи).
func wireRecipes(s *stack) {
	s.h.MetricRules = metric.NewRuleService(s.pool)
}

// recipesSeedProject — организация+проект под owner'ом; возвращает проект и
// cookie владельца (owner организации = оператор проекта).
func recipesSeedProject(t *testing.T, s *stack, slugPrefix string) (org.Project, *http.Cookie, *org.Service) {
	t.Helper()
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, slugPrefix+"-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), slugPrefix+"-co", "Recipes Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, slugPrefix+"-proj", "Recipes Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return proj, ownerCookie, orgSvc
}

// TestWebRecipesListPage — GET списка под оператором: 200, все 4 карточки
// рецептов с бейджем статуса данных (CH на стенде нет — у всех «ждём»);
// member без командного доступа — 404 (existence-oracle, как metrics).
func TestWebRecipesListPage(t *testing.T) {
	s := newStack(t)
	wireRecipes(s)
	proj, ownerCookie, orgSvc := recipesSeedProject(t, s, "rcp-list")

	authSvc := auth.NewService(s.pool)
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "rcp-list-member@example.com")
	// member организации БЕЗ команды, прикреплённой к проекту, — доступа нет.
	orgID := proj.OrgID
	if err := orgSvc.AddMember(context.Background(), orgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/recipes"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr := string(body)
	// Карточки рецептов идут внутри обёртки вертикального ритма .card-stack —
	// без неё section.card слипаются (ни .card, ни <section> margin не несут).
	stackAt := strings.Index(bodyStr, `<div class="card-stack">`)
	if stackAt < 0 {
		t.Errorf("GET %s: нет обёртки card-stack вокруг карточек рецептов", path)
	}
	for _, rec := range recipes.All() {
		marker := `data-recipe="` + rec.ID + `"`
		at := strings.Index(bodyStr, marker)
		if at < 0 {
			t.Errorf("GET %s: нет карточки %s (маркер %q)", path, rec.ID, marker)
		} else if stackAt >= 0 && at < stackAt {
			t.Errorf("GET %s: карточка %s стоит до открытия card-stack — вне стека", path, rec.ID)
		}
	}
	// Без ClickHouse на стенде статус у всех рецептов — «ждём данные»
	// (ровно 4 бейджа), «данные приходят» не встречается ни разу.
	if got := strings.Count(bodyStr, "Ждём данные"); got != len(recipes.All()) {
		t.Errorf("GET %s: бейджей «Ждём данные» = %d, want %d", path, got, len(recipes.All()))
	}
	if strings.Contains(bodyStr, "Данные приходят") {
		t.Errorf("GET %s: бейдж «Данные приходят» без единой метрики в CH", path)
	}

	resp = getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member, no team) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebRecipesDetailSnippet — страница redis: без ключа проекта сниппет
// скрыт с подсказкой, после выпуска ключа конфиг содержит сам ключ;
// неизвестный slug — 404.
func TestWebRecipesDetailSnippet(t *testing.T) {
	s := newStack(t)
	wireRecipes(s)
	proj, ownerCookie, orgSvc := recipesSeedProject(t, s, "rcp-detail")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/recipes/redis"

	// Ключа ещё нет — вместо сниппета подсказка «выпустите ключ».
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (no key) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "выпустите активный публичный ключ") {
		t.Errorf("GET %s (no key): нет подсказки про выпуск ключа: %s", path, body)
	}
	// Деталка тоже в стеке .card-stack: без него блок шагов и блок порогов
	// слипались, когда между ними не отрисованы графики (Charts == nil).
	if !strings.Contains(string(body), `<div class="card-stack">`) {
		t.Errorf("GET %s: нет обёртки card-stack между блоками деталки", path)
	}

	keys, err := orgSvc.CreateKeys(context.Background(), proj.ID, org.KindServer)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key := keys[0]
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, key.PublicKey) {
		t.Errorf("GET %s: сниппет не содержит ключ проекта %q", path, key.PublicKey)
	}
	if !strings.Contains(bodyStr, "receivers:") {
		t.Errorf("GET %s: нет YAML-конфига коллектора", path)
	}
	// Пороги redis ещё не созданы — все строки таблицы «будет создан».
	rec, _ := recipes.ByID("redis")
	if got := strings.Count(bodyStr, "Будет создан"); got != len(rec.Rules) {
		t.Errorf("GET %s: строк «Будет создан» = %d, want %d", path, got, len(rec.Rules))
	}

	resp = getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(proj.ID, 10)+"/recipes/nope", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET .../recipes/nope status = %d, want 404", resp.StatusCode)
	}
}

// TestWebRecipesDetailCanOperate — гейт кнопки «Создать рекомендованные
// пороги» (аудит UX/QA P1: зрителю кнопка вела в 404 requireProjectOperator).
//
// ВАЖНО про предикаты: сегодня CanOperate == CanAccessProject
// (canOperateProject в operate.go — прямой алиас, случая «доступ есть,
// оператор — нет» не существует), поэтому НАСТОЯЩИЙ участник команды с
// доступом к проекту видит кнопку, и живого пользователя с CanOperate=false
// на открытой странице не существует. HTTP-часть фиксирует это совпадение
// (участник команды видит кнопку), а ветку зрителя (hint вместо формы)
// проверяем прямым рендером шаблона с CanOperate=false — она сработает в
// тот момент, когда предикаты разойдутся.
func TestWebRecipesDetailCanOperate(t *testing.T) {
	s := newStack(t)
	wireRecipes(s)
	proj, _, orgSvc := recipesSeedProject(t, s, "rcp-oper")

	authSvc := auth.NewService(s.pool)
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "rcp-oper-member@example.com")
	if err := orgSvc.AddMember(context.Background(), proj.OrgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Доступ члена — через команду проекта (lvlAccess): member видит только
	// проекты своих команд.
	addTeamAccess(t, orgSvc, proj.OrgID, proj.ID, memberID, "rcp-oper-team")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/recipes/redis"
	resp := getWithCookie(t, s.srv, path, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (team member) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantBtn := i18n.T(ctx, "recipes.create_rules")
	if !strings.Contains(string(body), wantBtn) {
		t.Errorf("участник команды (CanOperate==CanAccessProject) не видит кнопку %q: %s", wantBtn, body)
	}
	if strings.Contains(string(body), i18n.T(ctx, "recipes.operator_only")) {
		t.Errorf("hint «только оператору» показан оператору: %s", body)
	}

	// Ветка зрителя — прямой рендер шаблона (как renderHostDetail):
	// CanOperate=false при незакрытых порогах — hint вместо формы POST.
	rec, _ := recipes.ByID("redis")
	vm := templates.RecipeDetailVM{
		ProjectID: proj.ID,
		Recipe:    rec,
		Statuses:  recipes.RuleStatuses(nil, rec),
	}
	var sb strings.Builder
	if err := templates.RecipeDetail(vm, "").Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	if !strings.Contains(html, i18n.T(ctx, "recipes.operator_only")) {
		t.Errorf("CanOperate=false: нет подсказки recipes.operator_only: %s", html)
	}
	if strings.Contains(html, "/thresholds") || strings.Contains(html, wantBtn) {
		t.Errorf("CanOperate=false: форма/кнопка создания порогов всё ещё в разметке: %s", html)
	}
}

// TestWebRecipesNilService — h.MetricRules не проведён (узкий тестовый
// стенд): без RuleService не посчитать статусы порогов, а POST создания
// мёртв — раздел целиком отвечает 404 (тот же nil-guard, что у
// TestWebAlertSuppressionNilService / escalationsPage / slosPage).
// wireRecipes здесь НАРОЧНО не зовётся.
func TestWebRecipesNilService(t *testing.T) {
	s := newStack(t)
	proj, ownerCookie, _ := recipesSeedProject(t, s, "rcp-nil")

	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/recipes"
	for _, path := range []string{base, base + "/redis"} {
		resp := getWithCookie(t, s.srv, path, ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s (nil MetricRules) status = %d, want 404", path, resp.StatusCode)
		}
	}

	resp := postForm(t, s.srv, base+"/redis/thresholds", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST %s/redis/thresholds (nil MetricRules) status = %d, want 404", base, resp.StatusCode)
	}
}

// TestWebRecipesThresholdsCreate — POST под оператором создаёт ровно
// len(Rules) правил и редиректит на страницу рецепта; ПОВТОРНЫЙ POST правил
// не добавляет (идемпотентность T3 через HTTP); member без командного
// доступа получает 404 (requireProjectOperator — единый existence-oracle:
// не-оператор не видит и сам проект, как у alert-suppression/escalations).
func TestWebRecipesThresholdsCreate(t *testing.T) {
	s := newStack(t)
	wireRecipes(s)
	proj, ownerCookie, orgSvc := recipesSeedProject(t, s, "rcp-post")

	authSvc := auth.NewService(s.pool)
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "rcp-post-member@example.com")
	if err := orgSvc.AddMember(context.Background(), proj.OrgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/recipes/redis"
	path := base + "/thresholds"

	// Не-оператор: правила не создаются, ответ — 404 (existence-oracle).
	resp := postForm(t, s.srv, path, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member, no team) status = %d, want 404", path, resp.StatusCode)
	}
	rules, err := s.h.MetricRules.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("после POST не-оператора правил = %d, want 0", len(rules))
	}

	rec, _ := recipes.ByID("redis")
	resp = postForm(t, s.srv, path, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	loc := resp.Header.Get("Location")
	flashVal := ""
	for _, c := range resp.Cookies() {
		if c.Name == "flash" {
			flashVal = c.Value
		}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", path, resp.StatusCode)
	}
	if loc != base {
		t.Errorf("POST %s Location = %q, want %q", path, loc, base)
	}
	// Флеш «создано N, пропущено M» уехал в redirect-cookie: created=3,
	// skipped=0 кодируется без хвоста |m (см. setFlash, парный формат).
	if got, err := url.QueryUnescape(flashVal); err != nil || got != "ok|flash.recipes_applied|3" {
		t.Errorf("flash-cookie после POST = %q (%v), want ok|flash.recipes_applied|3", got, err)
	}
	rules, err = s.h.MetricRules.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != len(rec.Rules) {
		t.Fatalf("после POST правил = %d, want %d", len(rules), len(rec.Rules))
	}

	// Повторный POST — правил ровно столько же (skip, не дубли).
	resp = postForm(t, s.srv, path, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("повторный POST %s status = %d, want 303", path, resp.StatusCode)
	}
	rules, err = s.h.MetricRules.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != len(rec.Rules) {
		t.Fatalf("после повторного POST правил = %d, want %d (идемпотентность)", len(rules), len(rec.Rules))
	}

	// Неизвестный slug — 404 и никаких новых правил (recipes.ByID до гейта).
	resp = postForm(t, s.srv, "/projects/"+strconv.FormatInt(proj.ID, 10)+"/recipes/nope/thresholds",
		url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST .../recipes/nope/thresholds status = %d, want 404", resp.StatusCode)
	}
	rules, err = s.h.MetricRules.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != len(rec.Rules) {
		t.Fatalf("после POST на неизвестный slug правил = %d, want %d", len(rules), len(rec.Rules))
	}
}
