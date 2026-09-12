package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func recipesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/recipes"
}

// Сигнатуры реестра — скалярный агрегат (gauge/не-monotonic sum), виден с первого скрейпа,
// без ожидания второй корзины rate-пути.
const recipeDetectWindow = 15 * time.Minute

// Ошибка ClickHouse — «данных нет» с логом, не падение: страница рецептов вспомогательная.
func (h *Handler) recipeDataArrives(ctx context.Context, projectID int64, rec recipes.Recipe) bool {
	if h.Metrics == nil {
		return false
	}
	now := time.Now()
	_, ok, err := h.Metrics.Aggregate(ctx, projectID, rec.Signature, "", "", nil, "avg", now.Add(-recipeDetectWindow), now)
	if err != nil {
		slog.Warn("recipes: signature detection failed", "project_id", projectID, "recipe", rec.ID, "error", err)
		return false
	}
	return ok
}

// Список рецептов проверяет ВСЕ сигнатуры разом — один запрос вместо recipeDataArrives
// на каждый рецепт (тот тянет ещё metricType и, для monotonic-счётчиков, второй запрос
// на rate); страница деталей одного рецепта продолжает использовать recipeDataArrives.
func (h *Handler) recipeSignaturesWithData(ctx context.Context, projectID int64, all []recipes.Recipe) map[string]bool {
	if h.Metrics == nil {
		return nil
	}
	signatures := make([]string, len(all))
	for i, rec := range all {
		signatures[i] = rec.Signature
	}
	now := time.Now()
	arrived, err := h.Metrics.NamesWithData(ctx, projectID, signatures, now.Add(-recipeDetectWindow), now)
	if err != nil {
		slog.Warn("recipes: signature detection failed", "project_id", projectID, "error", err)
		return nil
	}
	return arrived
}

func (h *Handler) recipesListPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// Без RuleService не посчитать статусы порогов, а POST создания без него мёртв — раздел
	// целиком отвечает 404.
	if h.MetricRules == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	// Один List на все рецепты: RuleStatuses — чистая функция над срезом, N+1 не возникает.
	existing, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	all := recipes.All()
	arrived := h.recipeSignaturesWithData(r.Context(), projectID, all)
	cards := make([]templates.RecipeCardVM, 0, len(all))
	for _, rec := range all {
		created := 0
		statuses := recipes.RuleStatuses(existing, rec)
		for _, st := range statuses {
			if st.Exists {
				created++
			}
		}
		cards = append(cards, templates.RecipeCardVM{
			ID:           rec.ID,
			DataArrives:  arrived[rec.Signature],
			CreatedRules: created,
			TotalRules:   len(statuses),
		})
	}
	_ = templates.RecipesList(projectID, cards, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) recipeDetailPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil {
		h.notFound(w, r)
		return
	}
	rec, ok := recipes.ByID(r.PathValue("slug"))
	if !ok {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	existing, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Страница открыта любому с доступом, кнопка создания — только оператору (POST и так
	// гейтится requireProjectOperator — это лишь честность разметки).
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	dataArrives := h.recipeDataArrives(r.Context(), projectID, rec)
	var charts []templates.RecipeChartVM
	if dataArrives {
		// Шаг — та же формула autoStep, что у metricDetail: метрики читают сырую metric_points,
		// шаг не мельче минуты, без выравнивания.
		now := time.Now()
		step := autoStep(recipeChartWindow, time.Minute, 0, metricChartBuckets)
		charts = h.recipeCharts(r.Context(), projectID, rec, now.Add(-recipeChartWindow), now, step)
	}
	vm := templates.RecipeDetailVM{
		ProjectID:   projectID,
		Recipe:      rec,
		DataArrives: dataArrives,
		Config:      h.recipeConfig(r.Context(), projectID, rec),
		Statuses:    recipes.RuleStatuses(existing, rec),
		Charts:      charts,
		CanOperate:  canOperate,
	}
	_ = templates.RecipeDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

// "" — ключа нет или чтение упало, страница не падает (подсказка «выпустите ключ»).
// server, а не agent: рецепты не регистрируют хост, agent-ключ дал бы лишнее право.
func (h *Handler) recipeConfig(ctx context.Context, projectID int64, rec recipes.Recipe) string {
	keys, err := h.Org.KeysForProject(ctx, projectID)
	if err != nil {
		slog.Warn("recipes: cannot list project keys", "project_id", projectID, "error", err)
		return ""
	}
	key := liveKeyFor(keys, org.KindServer)
	if key == "" {
		return ""
	}
	return rec.Config(h.BaseURL, key)
}

func (h *Handler) recipeThresholdsCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil {
		h.notFound(w, r)
		return
	}
	rec, ok := recipes.ByID(r.PathValue("slug"))
	if !ok {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	created, skipped, err := recipes.ApplyRules(r.Context(), h.MetricRules, projectID, rec)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOKPair(w, "flash.recipes_applied", created, skipped)
	http.Redirect(w, r, recipesPath(projectID)+"/"+rec.ID, http.StatusSeeOther)
}
