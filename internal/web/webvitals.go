package web

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// perfVitalChartBuckets — сколько корзин в мини-графике p75 web vital на панели
// эндпойнта (та же грубость, что и спарклайн p95 в списке эндпойнтов).
const perfVitalChartBuckets = 24

// webVitalsPanelNames — порядок vitals в панели Web Vitals на странице
// эндпойнта: сначала три Core Web Vitals (LCP/INP/CLS), затем FCP/TTFB.
var webVitalsPanelNames = []string{"lcp", "inp", "cls", "fcp", "ttfb"}

func webVitalsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/web-vitals"
}

// webVitalsList — GET /projects/{id}/web-vitals: таблица страниц (pageload-
// транзакций) с p75 LCP/INP/CLS и рейтингом. Только чтение; доступ —
// CanAccessProject, иначе 404 (тот же принцип, что и performanceList); h.Trace
// nil → 404, как в performanceList.
func (h *Handler) webVitalsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Trace может быть nil в стендах без трейсинга — тогда 404, как и при
	// отсутствии доступа (тот же приём, что в performanceList), а не паника.
	if h.Trace == nil {
		http.NotFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}

	window, period := perfPeriodWindow(r.URL.Query().Get("period"))
	environment := r.URL.Query().Get("environment")
	sortKey := r.URL.Query().Get("sort")

	now := time.Now().UTC()
	from := now.Add(-window)

	pages, err := h.Trace.WebVitalsPages(r.Context(), projectID, from, now, environment)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	environments, err := h.Trace.Environments(r.Context(), projectID, from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	sortPageVitals(pages, sortKey)

	filter := templates.PerfFilter{Period: period, Environment: environment, Sort: sortKey}
	_ = templates.WebVitalsList(projectID, pages, filter, environments, h.currentEmail(r)).
		Render(r.Context(), w)
}

// sortPageVitals сортирует список страниц по query-параметру sort. Дефолт
// (пустой/неизвестный) — число замеров LCP по убыванию (в этом же порядке их
// отдаёт trace.Query.WebVitalsPages, но пересортировать всё равно надо: с
// указанным sort порядок другой).
func sortPageVitals(pages []trace.PageVitals, sortKey string) {
	less := func(i, j int) bool { return pages[i].Count > pages[j].Count }
	switch sortKey {
	case "name":
		less = func(i, j int) bool { return pages[i].Transaction < pages[j].Transaction }
	case "lcp":
		less = func(i, j int) bool { return pages[i].LCP.P75 > pages[j].LCP.P75 }
	case "inp":
		less = func(i, j int) bool { return pages[i].INP.P75 > pages[j].INP.P75 }
	case "cls":
		less = func(i, j int) bool { return pages[i].CLS.P75 > pages[j].CLS.P75 }
	}
	sort.SliceStable(pages, less)
}

// vitalsPanel собирает панель Web Vitals эндпойнта: по каждому из пяти vitals —
// общий p75 за период с рейтингом (PageVitalsOne, один запрос с учётом фильтра
// окружения) и мини-график p75 во времени (VitalSeries с шагом chart). Панель
// показывается, только если хотя бы у одного из пяти vitals есть данные за
// период в текущем окружении (Rating != ""); иначе возвращает nil и панель не
// рендерится (в т.ч. когда vitals есть лишь в другом окружении — при
// environment=staging панели у чисто production-страницы не будет).
func (h *Handler) vitalsPanel(r *http.Request, projectID int64, transaction string, from, now time.Time, window time.Duration, environment string) ([]templates.VitalPanelRow, error) {
	lcp, inp, cls, fcp, ttfb, err := h.Trace.PageVitalsOne(r.Context(), projectID, transaction, from, now, environment)
	if err != nil {
		return nil, err
	}
	// Порядок совпадает с webVitalsPanelNames (lcp, inp, cls, fcp, ttfb).
	overall := []trace.Vital{lcp, inp, cls, fcp, ttfb}

	hasData := false
	for _, v := range overall {
		if v.Rating != "" {
			hasData = true
			break
		}
	}
	if !hasData {
		return nil, nil
	}

	chartStep := perfBucketStep(window, perfVitalChartBuckets)
	rows := make([]templates.VitalPanelRow, 0, len(webVitalsPanelNames))
	for i, name := range webVitalsPanelNames {
		series, err := h.Trace.VitalSeries(r.Context(), projectID, transaction, name, from, now, chartStep, environment)
		if err != nil {
			return nil, err
		}
		rows = append(rows, templates.VitalPanelRow{
			Vital: overall[i],
			Chart: vitalSeriesSVG(series, perfVitalChartWidth, perfVitalChartHeight),
		})
	}
	return rows, nil
}
