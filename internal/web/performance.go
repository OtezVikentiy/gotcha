package web

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// perfDefaultPeriod — пресет окна по умолчанию для perf-страниц (см.
// parseTimeRange). Пресеты и их окна теперь в едином timerange.go.
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
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Trace может быть nil в стендах без трейсинга — тогда 404, как и при
	// отсутствии доступа (тот же приём, что и guard на h.PerfIssues ниже), а не
	// паника при разыменовании.
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

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)
	environment := r.URL.Query().Get("environment")
	sortKey := canonicalEndpointSort(r.URL.Query().Get("sort"))

	from, now := tr.From, tr.To

	// Отказ ClickHouse — НЕ 500: фильтры и оболочка остаются, на месте
	// таблицы — «данные временно недоступны» (единый приём CH-страниц,
	// образец — logsList). Первый отказ прекращает опрос хранилища.
	stats, environments, latencyByTx, loadErr := h.performanceListData(r.Context(), projectID, from, now, tr.Window(), environment, sortKey, int(project.ApdexThresholdMS))
	loadFailed := loadErr != nil
	if loadFailed {
		slog.Warn("perf: endpoints list failed", "project_id", projectID, "err", loadErr)
	}

	// Усечение до top-N ПОСЛЕ сортировки и ДО сборки спарклайнов: спарклайн
	// каждой строки — отдельный CH-запрос, поэтому число строк ограничиваем
	// заранее. total (полное число эндпойнтов) отдаём в шаблон для пометки.
	total := len(stats)
	if len(stats) > perfEndpointLimit {
		stats = stats[:perfEndpointLimit]
	}
	rows := make([]templates.EndpointRow, len(stats))
	for i, st := range stats {
		rows[i] = templates.EndpointRow{
			Stat:      st,
			Sparkline: latencySparklineSVG(r.Context(), latencyByTx[st.Transaction], perfSparklineWidth, perfSparklineHeight),
		}
	}

	filter := templates.PerfFilter{Range: timeRangeVM(tr), Environment: environment, Sort: sortKey}
	_ = templates.PerformanceList(projectID, rows, total, filter, environments, int(project.ApdexThresholdMS),
		h.cardinalityNotices(projectID), h.currentEmail(r), loadFailed).
		Render(r.Context(), w)
}

// performanceListData — чтения списка транзакций из ClickHouse: сводка по
// эндпойнтам (уже отсортированная под sortKey), окружения и спарклайны.
// Спарклайн p95 на каждую строку — раньше это было по одному CH-запросу
// (EndpointLatency) на строку, до perfEndpointLimit последовательных
// round-trip'ов подряд на загрузку страницы. EndpointLatencyBatch читает все
// строки ОДНИМ запросом (WHERE transaction IN ?) по срезу первых
// perfEndpointLimit транзакций. Первая же ошибка возвращается как есть —
// вызывающий переводит страницу в состояние «данные недоступны».
func (h *Handler) performanceListData(ctx context.Context, projectID int64, from, now time.Time, window time.Duration, environment, sortKey string, apdexT int) ([]trace.EndpointStat, []string, map[string][]trace.LatencyPoint, error) {
	stats, err := h.Trace.Endpoints(ctx, projectID, from, now, environment, apdexT)
	if err != nil {
		return nil, nil, nil, err
	}
	environments, err := h.Trace.Environments(ctx, projectID, from, now)
	if err != nil {
		return nil, nil, nil, err
	}
	sortEndpointStats(stats, sortKey)

	head := stats
	if len(head) > perfEndpointLimit {
		head = head[:perfEndpointLimit]
	}
	step := perfBucketStep(window, perfSparklineBuckets)
	transactions := make([]string, len(head))
	for i, st := range head {
		transactions[i] = st.Transaction
	}
	latencyByTx, err := h.Trace.EndpointLatencyBatch(ctx, projectID, transactions, from, now, step, environment)
	if err != nil {
		return nil, nil, nil, err
	}
	return stats, environments, latencyByTx, nil
}

// canonicalEndpointSort приводит query-параметр sort к фактически применяемой
// колонке: пустой/незнакомый ключ означает сортировку по throughput (см.
// sortEndpointStats), и заголовок таблицы обязан показывать её aria-sort'ом и
// стрелкой уже с первого захода, а не только после явного клика (QA MINOR-5).
func canonicalEndpointSort(sortKey string) string {
	switch sortKey {
	case "name", "p50", "p75", "p95", "p99", "failure", "apdex":
		return sortKey
	}
	return "throughput"
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
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Trace может быть nil в стендах без трейсинга — тогда 404, а не паника
	// при разыменовании (см. performanceList).
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

	// Имя эндпойнта едет в пути %-экранированным (см. templates.endpointPath),
	// но ServeMux УЖЕ декодирует значение {transaction...} один раз перед
	// PathValue — поэтому здесь повторно декодировать НЕЛЬЗЯ (иначе имя с «%»,
	// например «%20» или «%beta», исказится и уйдёт за данными другого
	// эндпойнта). PathEscape один раз на ссылке ↔ ServeMux-decode один раз тут
	// корректно кругооборотят имя, включая литеральный «%».
	transaction := r.PathValue("transaction")
	if transaction == "" {
		h.notFound(w, r)
		return
	}

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)
	environment := r.URL.Query().Get("environment")

	from, now := tr.From, tr.To

	step := perfBucketStep(tr.Window(), perfLatencyBuckets)
	// Все чтения ClickHouse этой страницы — одним блоком: отказ хранилища
	// не роняет страницу (единый приём CH-страниц, образец — logsList), на
	// месте графиков, виталов и медленных трейсов — «данные временно
	// недоступны», шапка и связанные проблемы (PostgreSQL) остаются. Первый
	// отказ прекращает опрос хранилища.
	var (
		points    []trace.LatencyPoint
		histogram []trace.DurationBucket
		slowest   []trace.TraceRow
		vitals    []templates.VitalPanelRow
	)
	loadErr := func() error {
		var err error
		if points, err = h.Trace.EndpointLatency(r.Context(), projectID, transaction, from, now, step, environment); err != nil {
			return err
		}
		// Дозаполняем окно пустыми корзинами (Count==0 — разрыв линии/нет
		// столбика), чтобы оси латентности и трафика шли по выбранному
		// интервалу целиком.
		points = fillSeries(points, from, now, step,
			func(p trace.LatencyPoint) time.Time { return p.T },
			func(t time.Time) trace.LatencyPoint { return trace.LatencyPoint{T: t} })
		if histogram, err = h.Trace.DurationHistogram(r.Context(), projectID, transaction, from, now, environment, perfHistogramBuckets); err != nil {
			return err
		}
		if slowest, err = h.Trace.SlowestTraces(r.Context(), projectID, transaction, from, now, perfSlowestLimit); err != nil {
			return err
		}
		vitals, err = h.vitalsPanel(r, projectID, transaction, from, now, tr.Window(), environment)
		return err
	}()
	loadFailed := loadErr != nil
	if loadFailed {
		slog.Warn("perf: endpoint detail failed", "project_id", projectID, "transaction", transaction, "err", loadErr)
	}
	slowestRows := make([]templates.SlowestTraceRow, len(slowest))
	if h.SpanRetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(h.SpanRetentionDays) * 24 * time.Hour)
		for i, row := range slowest {
			slowestRows[i] = templates.SlowestTraceRow{
				Row:     row,
				Expired: row.Timestamp.Before(cutoff),
			}
		}
	} else {
		for i, row := range slowest {
			slowestRows[i] = templates.SlowestTraceRow{Row: row}
		}
	}

	// Связанные perf-проблемы этого эндпойнта: List отдаёт проблемы проекта, а
	// culprit (имя транзакции) фильтруем в Go — минимальный вариант без нового
	// метода IssueService. PerfIssues может быть nil в стендах, которым он не
	// нужен, — тогда секция просто пустая.
	var perfIssues []trace.PerfIssue
	if h.PerfIssues != nil {
		all, err := h.PerfIssues.List(r.Context(), projectID, "", perfIssuesLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		for _, iss := range all {
			if iss.Culprit == transaction {
				perfIssues = append(perfIssues, iss)
			}
		}
	}

	// Панель Web Vitals (этап 4, план 2, задача 2): только если у транзакции
	// есть хоть один web vital за период (иначе vitals == nil и панель не
	// рендерится).
	// Маркеры деплоев на графиках латентности и трафика (C5): выкладки этого
	// проекта в том же окне. nil-guard — стенды без деплоев не рисуют маркеров.
	var deploys []deploy.Deployment
	if h.Deploy != nil {
		deploys, _ = h.Deploy.List(r.Context(), projectID, from, now, 20)
	}

	data := templates.EndpointDetailData{
		ProjectID:    projectID,
		Transaction:  transaction,
		Range:        timeRangeVM(tr),
		Environment:  environment,
		ApdexT:       int(project.ApdexThresholdMS),
		LatencyChart: latencyLinesSVG(r.Context(), points, deploys, perfLatencyChartWidth, perfLatencyChartHeight),
		Throughput:   throughputBarsSVG(r.Context(), points, deploys, perfLatencyChartWidth, perfLatencyChartHeight),
		Histogram:    durationHistogramSVG(r.Context(), histogram, perfLatencyChartWidth, perfLatencyChartHeight),
		StepLabel:    formatStep(step),
		From:         endpointOrigin(r.URL.Query().Get("from")),
		Slowest:      slowestRows,
		PerfIssues:   perfIssues,
		Vitals:       vitals,
		LoadFailed:   loadFailed,
	}
	_ = templates.EndpointDetail(data, h.currentEmail(r)).Render(r.Context(), w)
}

// formatStep — шаг агрегации графиков для подписи под заголовком: «5m», «1h».
// Без него высота столбика трафика ни о чём не говорит.
func formatStep(step time.Duration) string {
	switch {
	case step >= time.Hour:
		return strconv.Itoa(int(step.Hours())) + "h"
	case step >= time.Minute:
		return strconv.Itoa(int(step.Minutes())) + "m"
	default:
		return strconv.Itoa(int(step.Seconds())) + "s"
	}
}

// endpointOrigin — подраздел, из которого пришли на страницу эндпойнта.
// Значение приходит из адреса, поэтому сверяется со списком известных: в
// шаблон не должна попадать произвольная строка из query.
func endpointOrigin(from string) string {
	if from == "web-vitals" {
		return from
	}
	return ""
}
