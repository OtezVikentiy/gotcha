package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func recipesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/recipes"
}

// recipeDetectWindow — окно детекции «данные приходят»: сигнатурная метрика
// рецепта агрегируется за последние 15 минут (спека B6 §4). Сигнатуры
// подобраны в реестре так, что скалярный агрегат виден с первого же скрейпа
// (gauge / не-monotonic sum), поэтому статус загорается без ожидания второй
// корзины rate-пути.
const recipeDetectWindow = 15 * time.Minute

// recipeDataArrives — детекция «данные приходят» по сигнатурной метрике
// рецепта. Ошибка ClickHouse (или непроведённый h.Metrics на узком стенде)
// трактуется как «данных нет» с логом — страница рецептов вспомогательная и
// не должна падать из-за недоступной аналитики; тот же приём, что подсказки
// имён метрик в renderMetricAlerts.
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

// recipesListPage — GET /projects/{id}/recipes: карточки всех рецептов с
// бейджем статуса данных и счётчиком созданных порогов. Доступ — любой с
// доступом к проекту (CanAccessProject), как metricsList: это чтение
// телеметрии и справка по подключению, не настройка.
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
	// Nil-guard как у metricAlertsPage: без RuleService не посчитать статусы
	// порогов, а POST создания без него мёртв — раздел целиком отвечает 404.
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
	// Один List на все четыре рецепта: RuleStatuses — чистая функция над уже
	// загруженным срезом, N+1 по правилам не возникает.
	existing, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	all := recipes.All()
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
			DataArrives:  h.recipeDataArrives(r.Context(), projectID, rec),
			CreatedRules: created,
			TotalRules:   len(statuses),
		})
	}
	_ = templates.RecipesList(projectID, cards, h.currentEmail(r)).Render(r.Context(), w)
}

// recipeDetailPage — GET /projects/{id}/recipes/{slug}: шаги подключения со
// сниппетом конфига (ключ проекта — тем же путём KeysForProject→firstLiveKey,
// что hostInstallBlocks; нет живого ключа → сниппет скрыт с подсказкой),
// статус данных, преднастроенные графики (только когда данные уже приходят —
// до первого скрейпа блок из одних пустых карточек лишь загромождал бы
// инструкцию подключения) и таблица рекомендованных порогов. Доступ — как у
// списка.
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
	// CanOperate — read-only, тот же приём, что hostDetail (renderHostDetail):
	// страница открыта любому с доступом, а кнопка создания порогов у
	// не-оператора не рендерится вовсе (POST и так гейтится
	// requireProjectOperator — тут только честность разметки: кнопка,
	// ведущая зрителя в 404, хуже подсказки).
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	dataArrives := h.recipeDataArrives(r.Context(), projectID, rec)
	var charts []templates.RecipeChartVM
	if dataArrives {
		// Диапазон ФИКСИРОВАННЫЙ — recipeChartWindow от реального «сейчас»,
		// НЕ resolveTimeRange (см. докблок константы: страница рецепта не
		// должна молча уезжать за глобальной кукой диапазона). Шаг — той же
		// формулой autoStep, что metricDetail: метрики читают сырую
		// metric_points, шаг не мельче минуты, без выравнивания.
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

// recipeConfig — готовый сниппет коллектора с первым активным публичным
// ключом проекта; "" — ключа нет или чтение ключей упало (страница не
// падает, шаблон показывает подсказку «выпустите ключ» со ссылкой на
// настройки) — ровно контракт hostInstallBlocks (hosts.go).
func (h *Handler) recipeConfig(ctx context.Context, projectID int64, rec recipes.Recipe) string {
	keys, err := h.Org.KeysForProject(ctx, projectID)
	if err != nil {
		slog.Warn("recipes: cannot list project keys", "project_id", projectID, "error", err)
		return ""
	}
	key := firstLiveKey(keys)
	if key == "" {
		return ""
	}
	return rec.Config(h.BaseURL, key)
}

// recipeThresholdsCreate — POST /projects/{id}/recipes/{slug}/thresholds:
// идемпотентно создать недостающие рекомендованные пороги рецепта
// (recipes.ApplyRules поверх того же RuleService, что форма metric alerts).
// Доступ — оператор проекта (requireProjectOperator), как у ручных мутаций
// правил; не-оператору отвечает единый existence-oracle 404.
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
