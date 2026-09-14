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
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const perfDefaultPeriod = "24h"

const (
	perfSparklineBuckets     = 24
	perfHistogramBuckets     = 20
	perfLatencyBuckets       = 48
	perfSlowestLimit         = 10
	perfIssuesByCulpritLimit = 20
)

// На каждую строку идёт отдельный CH-запрос спарклайна p95 — без потолка
// высококардинальный проект дал бы тысячи round-trip'ов на загрузку страницы.
const perfEndpointLimit = 100

// Шаг кратен 5 минутам и не меньше их: тогда trace.Query читает из дешёвой MV
// transactions_5m, а не из сырых transactions.
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
	// h.Trace может быть nil в стендах без трейсинга — 404, не паника при разыменовании.
	if h.Trace == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)
	environment := r.URL.Query().Get("environment")
	sortKey := canonicalEndpointSort(r.URL.Query().Get("sort"))

	from, now := tr.From, tr.To

	// Отказ ClickHouse — НЕ 500: фильтры остаются, на месте таблицы — «данные
	// временно недоступны».
	stats, environments, latencyByTx, capped, loadErr := h.performanceListData(r.Context(), projectID, from, now, tr.Window(), environment, sortKey, int(project.ApdexThresholdMS))
	loadFailed := loadErr != nil
	if loadFailed {
		slog.Warn("perf: endpoints list failed", "project_id", projectID, "err", loadErr)
	}

	// Усечение до top-N после сортировки и до сборки спарклайнов: те — отдельный CH-запрос на строку.
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
		h.cardinalityNotices(projectID), h.currentEmail(r), loadFailed, capped).
		Render(r.Context(), w)
}

// EndpointLatencyBatch читает все строки ОДНИМ запросом (WHERE transaction IN ?)
// вместо отдельного round-trip'а на каждую из первых perfEndpointLimit транзакций.
// capped — trace.Query.Endpoints упёрся в endpointsRowCap: строки за пределами потолка
// не попали в stats вовсе, независимо от perfEndpointLimit ниже.
func (h *Handler) performanceListData(ctx context.Context, projectID int64, from, now time.Time, window time.Duration, environment, sortKey string, apdexT int) (stats []trace.EndpointStat, environments []string, latencyByTx map[string][]trace.LatencyPoint, capped bool, err error) {
	stats, capped, err = h.Trace.Endpoints(ctx, projectID, from, now, environment, apdexT)
	if err != nil {
		return nil, nil, nil, false, err
	}
	environments, err = h.Trace.Environments(ctx, projectID, from, now)
	if err != nil {
		return nil, nil, nil, false, err
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
	latencyByTx, err = h.Trace.EndpointLatencyBatch(ctx, projectID, transactions, from, now, step, environment)
	if err != nil {
		return nil, nil, nil, false, err
	}
	return stats, environments, latencyByTx, capped, nil
}

// Пустой/незнакомый ключ означает сортировку по throughput — заголовок таблицы должен
// показать это aria-sort'ом уже с первого захода, не только после явного клика.
func canonicalEndpointSort(sortKey string) string {
	switch sortKey {
	case "name", "p50", "p75", "p95", "p99", "failure", "apdex":
		return sortKey
	}
	return "throughput"
}

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
	if h.Trace == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	// ServeMux уже декодирует {transaction...} один раз перед PathValue — повторное
	// декодирование тут исказит имя с «%» и уведёт за данными другого эндпойнта.
	transaction := r.PathValue("transaction")
	if transaction == "" {
		h.notFound(w, r)
		return
	}

	project, err := h.Org.GetProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)
	environment := r.URL.Query().Get("environment")

	from, now := tr.From, tr.To

	step := perfBucketStep(tr.Window(), perfLatencyBuckets)
	// Отказ ClickHouse не роняет страницу: шапка и связанные проблемы (PostgreSQL)
	// остаются, на месте графиков/виталов/трейсов — «данные временно недоступны».
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
		// Дозаполняем окно пустыми корзинами (Count==0), чтобы оси шли по интервалу целиком.
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

	var perfIssues []trace.PerfIssue
	if h.PerfIssues != nil {
		perfIssues, err = h.PerfIssues.List(r.Context(), projectID, "", transaction, perfIssuesByCulpritLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

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

// Сверяем со списком известных значений: в шаблон не должна попадать произвольная строка из query.
func endpointOrigin(from string) string {
	if from == "web-vitals" {
		return from
	}
	return ""
}
