package templates

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
)

// mustRecipe — рецепт из реестра по slug'у; тесты рендера гоняют настоящие
// рецепты (а не рукодельные Recipe{}), чтобы switch пер-рецептных подсказок
// в recipes.templ исполнялся ровно теми ID, что живут в продукте.
func mustRecipe(t *testing.T, id string) recipes.Recipe {
	t.Helper()
	rec, ok := recipes.ByID(id)
	if !ok {
		t.Fatalf("рецепт %q не найден в реестре", id)
	}
	return rec
}

// TestRecipeDetailDockerNoRules — docker: единственный рецепт без порогов.
// Вместо таблицы — пояснение с рабочей ссылкой на правила метрик; сниппет
// со своей socket-подсказкой; общий assumption-хинт и ссылка на /docs/recipes.
func TestRecipeDetailDockerNoRules(t *testing.T) {
	rec := mustRecipe(t, "docker")
	out := renderTo(t, RecipeDetail(RecipeDetailVM{
		ProjectID:  7,
		Recipe:     rec,
		Config:     "receivers:\n  docker_stats:\n",
		Statuses:   recipes.RuleStatuses(nil, rec),
		CanOperate: true,
	}, "u@e.com"))
	if !strings.Contains(out, "нет рекомендованных порогов") {
		t.Error("docker без Rules должен показывать пояснение recipes.docker.no_rules")
	}
	if !strings.Contains(out, "По метрикам") {
		t.Error("пояснение docker должно нести рабочую ссылку на страницу правил метрик")
	}
	if !strings.Contains(out, "докер-сокету") {
		t.Error("сниппет docker должен сопровождаться socket_note-подсказкой")
	}
	if !strings.Contains(out, "один инстанс сервиса на проект") {
		t.Error("страница рецепта должна нести assumption-хинт")
	}
	if !strings.Contains(out, `href="/docs/recipes"`) {
		t.Error("страница рецепта должна ссылаться на гайд /docs/recipes")
	}
	// «Данные приходят» встречается и в тексте шага restart — различаем
	// именно бейдж по его классу.
	if !strings.Contains(out, "Ждём данные") || strings.Contains(out, `badge-good">Данные приходят`) {
		t.Error("DataArrives=false — бейдж «Ждём данные», не «Данные приходят»")
	}
	if strings.Contains(out, "Создать рекомендованные пороги") {
		t.Error("у docker нечего создавать — кнопки порогов быть не должно")
	}
}

// TestRecipeDetailSnippetHints — пер-рецептные подсказки предусловий рядом со
// сниппетом (postgres/nginx/redis) и ветка «нет живого ключа»: сниппет скрыт,
// вместо него причина со ссылкой на настройки проекта.
func TestRecipeDetailSnippetHints(t *testing.T) {
	cases := []struct {
		id, hint string
	}{
		{"postgres", "pg_monitor"},
		{"nginx", "stub_status"},
		{"redis", "requirepass"},
	}
	for _, tc := range cases {
		rec := mustRecipe(t, tc.id)
		out := renderTo(t, RecipeDetail(RecipeDetailVM{
			ProjectID:  7,
			Recipe:     rec,
			Config:     "receivers: {}",
			Statuses:   recipes.RuleStatuses(nil, rec),
			CanOperate: true,
		}, "u@e.com"))
		if !strings.Contains(out, tc.hint) {
			t.Errorf("%s со сниппетом: нет пер-рецептной подсказки (%q)", tc.id, tc.hint)
		}
		if strings.Contains(out, "выпустите активный публичный ключ") {
			t.Errorf("%s со сниппетом: подсказка no_key не должна рендериться", tc.id)
		}
	}

	// Без живого ключа: сниппета и его подсказки нет, есть причина + ссылка.
	rec := mustRecipe(t, "nginx")
	out := renderTo(t, RecipeDetail(RecipeDetailVM{
		ProjectID:  7,
		Recipe:     rec,
		Statuses:   recipes.RuleStatuses(nil, rec),
		CanOperate: true,
	}, "u@e.com"))
	if !strings.Contains(out, "выпустите активный публичный ключ") {
		t.Error("Config==\"\": должна рендериться причина «выпустите ключ»")
	}
	if !strings.Contains(out, `href="/projects/7/settings"`) {
		t.Error("причина no_key должна вести в настройки проекта")
	}
	// stub_status встречается и в desc рецепта — отличаем именно подсказку.
	if strings.Contains(out, "включите её в конфиге nginx") {
		t.Error("без сниппета не должно быть и его подсказки предусловия")
	}
}

// TestRecipeDetailCharts — блок преднастроенных графиков: непустой график с
// легендой из двух рядов, подписью top-N усечения и ссылкой «открыть в
// метриках»; рядом Empty-график с пустым состоянием без легенды и ссылки.
// DataArrives=true заодно исполняет «зелёную» ветку бейджа данных.
func TestRecipeDetailCharts(t *testing.T) {
	rec := mustRecipe(t, "redis")
	charts := []RecipeChartVM{
		{
			Key:      "keyspace",
			TitleKey: "recipes.redis.chart.keyspace",
			Chart:    stub(),
			Legend: []LegendItem{
				{Label: "hits", Class: "legend-m1"},
				{Label: "misses", Class: "legend-m2"},
			},
			Truncated:   true,
			ExplorerURL: "/projects/7/metrics/redis.keyspace.hits",
		},
		{
			Key:      "memory",
			TitleKey: "recipes.redis.chart.memory",
			Empty:    true,
		},
	}
	out := renderTo(t, RecipeDetail(RecipeDetailVM{
		ProjectID:   7,
		Recipe:      rec,
		DataArrives: true,
		Config:      "receivers: {}",
		Statuses:    recipes.RuleStatuses(nil, rec),
		Charts:      charts,
		CanOperate:  true,
	}, "u@e.com"))
	for _, marker := range []string{`data-chart="keyspace"`, `data-chart="memory"`} {
		if !strings.Contains(out, marker) {
			t.Errorf("нет карточки графика %s", marker)
		}
	}
	if !strings.Contains(out, "<svg data-stub></svg>") {
		t.Error("непустой график должен отрендерить переданный Chart-компонент")
	}
	if !strings.Contains(out, "hits") || !strings.Contains(out, "legend-m2") {
		t.Error("легенда непустого графика должна отрендерить оба ряда")
	}
	if !strings.Contains(out, "самых крупных групп") {
		t.Error("Truncated=true — обязана быть подпись top-N усечения")
	}
	if !strings.Contains(out, `href="/projects/7/metrics/redis.keyspace.hits"`) {
		t.Error("нет ссылки «открыть в метриках»")
	}
	if !strings.Contains(out, "Данных для этого графика ещё нет") {
		t.Error("Empty-график должен показать пустое состояние")
	}
	if !strings.Contains(out, `badge-good">Данные приходят`) {
		t.Error("DataArrives=true — бейдж «Данные приходят»")
	}
}

// TestRecipeDetailThresholdStatuses — таблица порогов: пока есть pending —
// строки «Будет создан» и форма POST у оператора; когда все созданы — бейджи
// «Создан», вместо кнопки честное «все созданы».
func TestRecipeDetailThresholdStatuses(t *testing.T) {
	rec := mustRecipe(t, "redis")

	pending := renderTo(t, RecipeDetail(RecipeDetailVM{
		ProjectID:  7,
		Recipe:     rec,
		Config:     "receivers: {}",
		Statuses:   recipes.RuleStatuses(nil, rec),
		CanOperate: true,
	}, "u@e.com"))
	if got := strings.Count(pending, "Будет создан"); got != len(rec.Rules) {
		t.Errorf("строк «Будет создан» = %d, want %d", got, len(rec.Rules))
	}
	if !strings.Contains(pending, `action="/projects/7/recipes/redis/thresholds"`) {
		t.Error("оператору с pending-порогами должна рендериться форма POST создания")
	}
	if strings.Contains(pending, "Все рекомендованные пороги уже созданы") {
		t.Error("при pending-порогах подписи «все созданы» быть не должно")
	}

	statuses := recipes.RuleStatuses(nil, rec)
	for i := range statuses {
		statuses[i].Exists = true
	}
	done := renderTo(t, RecipeDetail(RecipeDetailVM{
		ProjectID:  7,
		Recipe:     rec,
		Config:     "receivers: {}",
		Statuses:   statuses,
		CanOperate: true,
	}, "u@e.com"))
	if got := strings.Count(done, "Создан<"); got != len(rec.Rules) {
		t.Errorf("бейджей «Создан» = %d, want %d", got, len(rec.Rules))
	}
	if !strings.Contains(done, "Все рекомендованные пороги уже созданы") {
		t.Error("когда все пороги существуют — подпись recipes.all_created")
	}
	if strings.Contains(done, `action="/projects/7/recipes/redis/thresholds"`) {
		t.Error("когда создавать нечего, формы POST быть не должно")
	}
}

// TestRecipesListCards — список: карточка с порогами несёт счётчик
// «создано X из Y», карточка без порогов (docker) — честное «без
// рекомендованных порогов»; бейдж данных — по DataArrives карточки.
func TestRecipesListCards(t *testing.T) {
	cards := []RecipeCardVM{
		{ID: "redis", DataArrives: true, CreatedRules: 2, TotalRules: 3},
		{ID: "docker", DataArrives: false, CreatedRules: 0, TotalRules: 0},
	}
	out := renderTo(t, RecipesList(7, cards, "u@e.com"))
	for _, marker := range []string{`data-recipe="redis"`, `data-recipe="docker"`} {
		if !strings.Contains(out, marker) {
			t.Errorf("нет карточки %s", marker)
		}
	}
	if !strings.Contains(out, "Порогов создано: 2 из 3") {
		t.Error("карточка с порогами должна нести счётчик «создано 2 из 3»")
	}
	if !strings.Contains(out, "Без рекомендованных порогов") {
		t.Error("карточка docker должна честно говорить об отсутствии порогов")
	}
	if !strings.Contains(out, "Данные приходят") || !strings.Contains(out, "Ждём данные") {
		t.Error("бейджи данных должны отражать DataArrives пер-карточно")
	}
	if !strings.Contains(out, `href="/projects/7/recipes/redis"`) {
		t.Error("заголовок карточки должен вести на страницу рецепта")
	}
}
