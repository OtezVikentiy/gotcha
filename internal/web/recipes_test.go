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
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
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
	for _, rec := range recipes.All() {
		marker := `data-recipe="` + rec.ID + `"`
		if !strings.Contains(bodyStr, marker) {
			t.Errorf("GET %s: нет карточки %s (маркер %q)", path, rec.ID, marker)
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

	key, err := orgSvc.CreateKey(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", path, resp.StatusCode)
	}
	if loc != base {
		t.Errorf("POST %s Location = %q, want %q", path, loc, base)
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
}
