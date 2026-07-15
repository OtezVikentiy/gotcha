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

// perfPeriods — окна списка/страницы эндпойнтов: query-параметр period →
// длительность окна. Дефолт — 24h (см. perfDefaultPeriod).
var perfPeriods = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

const perfDefaultPeriod = "24h"

// perfSparklineBuckets — сколько корзин в спарклайне p95 списка (та же грубость,
// что и полоска доступности мониторов). perfHistogramBuckets — столбиков в
// гистограмме длительностей на странице эндпойнта. perfLatencyBuckets — точек в
// графиках перцентилей/throughput на странице эндпойнта. perfSlowestLimit — сколько
// самых медленных трейсов показывать. perfIssuesLimit — сколько связанных
// perf-проблем эндпойнта запрашивать (фильтр по culprit — в Go).
const (
	perfSparklineBuckets = 24
	perfHistogramBuckets = 20
	perfLatencyBuckets   = 48
	perfSlowestLimit     = 10
	perfIssuesLimit      = 100
)

// perfEndpointLimit — сколько эндпойнтов показывать в списке (после сортировки).
// На каждую строку идёт отдельный CH-запрос спарклайна p95, поэтому у
// высококардинального проекта (непараметризованные роуты — ровно то, о чём
// предупреждает perf-мониторинг) без потолка получились бы тысячи
// последовательных round-trip'ов на загрузку страницы. Усечение раскрывается в
// UI, как и потолок waterfall.
const perfEndpointLimit = 100

// perfPeriodWindow возвращает длительность окна для query-параметра period и его
// нормализованное имя (неизвестное/пустое → дефолт).
func perfPeriodWindow(period string) (time.Duration, string) {
	if d, ok := perfPeriods[period]; ok {
		return d, period
	}
	return perfPeriods[perfDefaultPeriod], perfDefaultPeriod
}

// perfBucketStep выбирает шаг корзины для окна window и числа корзин buckets так,
// чтобы он был кратен 5 минутам (тогда trace.Query читает из дешёвой MV
// transactions_5m, а не из сырых transactions) и не меньше 5 минут.
func perfBucketStep(window time.Duration, buckets int) time.Duration {
	step := window / time.Duration(buckets)
	if step < 5*time.Minute {
		step = 5 * time.Minute
	}
	if r := step % (5 * time.Minute); r != 0 {
		step -= r
	}
	return step
}

func performancePath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/performance"
}

// performanceList — GET /projects/{id}/performance: таблица эндпойнтов проекта
// (доступ — CanAccessProject, иначе 404, тот же принцип, что и у monitorsList).
func (h *Handler) performanceList(w http.ResponseWriter, r *http.Request) {
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
	// отсутствии доступа (тот же приём, что и guard на h.PerfIssues ниже), а не
	// паника при разыменовании.
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

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	window, period := perfPeriodWindow(r.URL.Query().Get("period"))
	environment := r.URL.Query().Get("environment")
	sortKey := r.URL.Query().Get("sort")

	now := time.Now().UTC()
	from := now.Add(-window)

	stats, err := h.Trace.Endpoints(r.Context(), projectID, from, now, environment, int(project.ApdexThresholdMS))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	environments, err := h.Trace.Environments(r.Context(), projectID, from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	sortEndpointStats(stats, sortKey)

	// Усечение до top-N ПОСЛЕ сортировки и ДО сборки спарклайнов: спарклайн
	// каждой строки — отдельный CH-запрос, поэтому число строк ограничиваем
	// заранее. total (полное число эндпойнтов) отдаём в шаблон для пометки.
	total := len(stats)
	if len(stats) > perfEndpointLimit {
		stats = stats[:perfEndpointLimit]
	}

	step := perfBucketStep(window, perfSparklineBuckets)
	rows := make([]templates.EndpointRow, len(stats))
	for i, st := range stats {
		points, err := h.Trace.EndpointLatency(r.Context(), projectID, st.Transaction, from, now, step, environment)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		rows[i] = templates.EndpointRow{
			Stat:      st,
			Sparkline: latencySparklineSVG(points, perfSparklineWidth, perfSparklineHeight),
		}
	}

	filter := templates.PerfFilter{Period: period, Environment: environment, Sort: sortKey}
	_ = templates.PerformanceList(projectID, rows, total, filter, environments, int(project.ApdexThresholdMS), h.currentEmail(r)).
		Render(r.Context(), w)
}

// sortEndpointStats сортирует список эндпойнтов по query-параметру sort. Дефолт
// (пустой/неизвестный) — throughput по убыванию (в этом порядке их и отдаёт
// trace.Query.Endpoints, но пересортировать всё равно надо: с указанным sort
// порядок другой).
func sortEndpointStats(stats []trace.EndpointStat, sortKey string) {
	less := func(i, j int) bool { return stats[i].Throughput > stats[j].Throughput }
	switch sortKey {
	case "name":
		less = func(i, j int) bool { return stats[i].Transaction < stats[j].Transaction }
	case "p50":
		less = func(i, j int) bool { return stats[i].P50 > stats[j].P50 }
	case "p75":
		less = func(i, j int) bool { return stats[i].P75 > stats[j].P75 }
	case "p95":
		less = func(i, j int) bool { return stats[i].P95 > stats[j].P95 }
	case "p99":
		less = func(i, j int) bool { return stats[i].P99 > stats[j].P99 }
	case "failure":
		less = func(i, j int) bool { return stats[i].FailureRate > stats[j].FailureRate }
	case "apdex":
		less = func(i, j int) bool { return stats[i].ApdexScore < stats[j].ApdexScore }
	}
	sort.SliceStable(stats, less)
}

// endpointDetail — GET /projects/{id}/performance/{transaction}: страница
// эндпойнта. transaction — имя, %-экранированное в ссылке и уже раскодированное
// ServeMux (может содержать слэши и произвольные символы). Доступ —
// CanAccessProject, иначе 404.
func (h *Handler) endpointDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Trace может быть nil в стендах без трейсинга — тогда 404, а не паника
	// при разыменовании (см. performanceList).
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

	// Имя эндпойнта едет в пути %-экранированным (см. templates.endpointPath),
	// но ServeMux УЖЕ декодирует значение {transaction...} один раз перед
	// PathValue — поэтому здесь повторно декодировать НЕЛЬЗЯ (иначе имя с «%»,
	// например «%20» или «%beta», исказится и уйдёт за данными другого
	// эндпойнта). PathEscape один раз на ссылке ↔ ServeMux-decode один раз тут
	// корректно кругооборотят имя, включая литеральный «%».
	transaction := r.PathValue("transaction")
	if transaction == "" {
		http.NotFound(w, r)
		return
	}

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	window, period := perfPeriodWindow(r.URL.Query().Get("period"))
	environment := r.URL.Query().Get("environment")

	now := time.Now().UTC()
	from := now.Add(-window)

	step := perfBucketStep(window, perfLatencyBuckets)
	points, err := h.Trace.EndpointLatency(r.Context(), projectID, transaction, from, now, step, environment)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	histogram, err := h.Trace.DurationHistogram(r.Context(), projectID, transaction, from, now, environment, perfHistogramBuckets)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	slowest, err := h.Trace.SlowestTraces(r.Context(), projectID, transaction, from, now, perfSlowestLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Связанные perf-проблемы этого эндпойнта: List отдаёт проблемы проекта, а
	// culprit (имя транзакции) фильтруем в Go — минимальный вариант без нового
	// метода IssueService. PerfIssues может быть nil в стендах, которым он не
	// нужен, — тогда секция просто пустая.
	var perfIssues []trace.PerfIssue
	if h.PerfIssues != nil {
		all, err := h.PerfIssues.List(r.Context(), projectID, "", perfIssuesLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		for _, iss := range all {
			if iss.Culprit == transaction {
				perfIssues = append(perfIssues, iss)
			}
		}
	}

	data := templates.EndpointDetailData{
		ProjectID:    projectID,
		Transaction:  transaction,
		Period:       period,
		Environment:  environment,
		ApdexT:       int(project.ApdexThresholdMS),
		LatencyChart: latencyLinesSVG(points, perfLatencyChartWidth, perfLatencyChartHeight),
		Throughput:   throughputBarsSVG(points, perfLatencyChartWidth, perfLatencyChartHeight),
		Histogram:    durationHistogramSVG(histogram, perfLatencyChartWidth, perfLatencyChartHeight),
		Slowest:      slowest,
		PerfIssues:   perfIssues,
	}
	_ = templates.EndpointDetail(data, h.currentEmail(r)).Render(r.Context(), w)
}
