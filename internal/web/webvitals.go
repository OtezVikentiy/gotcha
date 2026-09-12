package web

import (
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const perfVitalChartBuckets = 24

// сначала три Core Web Vitals (LCP/INP/CLS), затем FCP/TTFB.
var webVitalsPanelNames = []string{"lcp", "inp", "cls", "fcp", "ttfb"}

func webVitalsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/web-vitals"
}

func (h *Handler) webVitalsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Trace может быть nil в стендах без трейсинга — 404, не паника.
	if h.Trace == nil {
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

	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)
	environment := r.URL.Query().Get("environment")
	sortKey := r.URL.Query().Get("sort")

	from, now := tr.From, tr.To

	// отказ ClickHouse — не 500: фильтры/оболочка остаются, вместо таблицы
	// «данные временно недоступны».
	pages, err := h.Trace.WebVitalsPages(r.Context(), projectID, from, now, environment)
	var environments []string
	if err == nil {
		environments, err = h.Trace.Environments(r.Context(), projectID, from, now)
	}
	loadFailed := err != nil
	if loadFailed {
		slog.Warn("perf: web vitals list failed", "project_id", projectID, "err", err)
		pages, environments = nil, nil
	}

	sortPageVitals(pages, sortKey)

	filter := templates.PerfFilter{Range: timeRangeVM(tr), Environment: environment, Sort: sortKey}
	_ = templates.WebVitalsList(projectID, pages, filter, environments, h.currentEmail(r), loadFailed).
		Render(r.Context(), w)
}

// дефолт даёт тот же порядок, что уже отдаёт WebVitalsPages, но пересортировка
// всё равно нужна для остальных значений sortKey.
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

// панель рендерится, только если хотя бы у одного vital есть данные в текущем
// окружении — иначе nil, даже если vitals есть лишь в другом окружении.
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
			Chart: vitalSeriesSVG(r.Context(), series, perfVitalChartWidth, perfVitalChartHeight,
				vitalValueFormatter(name)),
		})
	}
	return rows, nil
}

// та же запись, что у значения в строке таблицы — иначе подсказка спарклайна
// показывала бы голое число без единицы измерения.
func vitalValueFormatter(name string) func(float64) string {
	if name == "cls" {
		return func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	}
	return func(ms float64) string {
		if ms < 1000 {
			return strconv.FormatFloat(ms, 'f', 0, 64) + "ms"
		}
		return strconv.FormatFloat(ms/1000, 'f', 2, 64) + "s"
	}
}
