package web

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const systemMetricPrefix = "system."

func filterSystemMetrics(metrics []metric.MetricInfo, showSystem bool) (visible []metric.MetricInfo, hiddenCount int) {
	for _, m := range metrics {
		if strings.HasPrefix(m.Name, systemMetricPrefix) {
			hiddenCount++
			if !showSystem {
				continue
			}
		}
		visible = append(visible, m)
	}
	return visible, hiddenCount
}

func metricsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/metrics"
}

// autoStep по окну (не мельче минуты): 1ч→~1м, 24ч→~12м, 7д→~1.4ч, 30д→~6ч.
const metricChartBuckets = 120

func (h *Handler) metricsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Metrics == nil {
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
	environment := r.URL.Query().Get("environment")
	metrics, err := h.Metrics.ListMetrics(r.Context(), projectID, environment)
	// Отказ ClickHouse — не 500: список остаётся, «данные временно недоступны».
	loadFailed := err != nil
	if loadFailed {
		slog.Warn("metrics: list failed", "project_id", projectID, "err", err)
		metrics = nil
	}
	showSystem := r.URL.Query().Get("system") == "1"
	visible, hiddenCount := filterSystemMetrics(metrics, showSystem)
	_ = templates.MetricsList(projectID, visible, environment, h.currentEmail(r), showSystem, hiddenCount, loadFailed).Render(r.Context(), w)
}

// ширина завязана на класс chart-vb<ширина> (кегль подписей осей в app.css);
// связь проверяет TestChartViewBoxFontSizeRules по имени константы, не по литералу.
const (
	metricChartWidth  = 720
	metricChartHeight = 200
)

func (h *Handler) metricDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Metrics == nil {
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
	name := r.PathValue("name")

	info, found, err := h.Metrics.MetricInfoByName(r.Context(), projectID, name)
	if err != nil {
		// found=false БЕЗ ошибки означает отсутствие метрики (404 ниже); err != nil — отказ хранилища.
		h.renderMetricUnavailable(w, r, projectID, name, err)
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}

	tr := h.resolveTimeRange(w, r, "24h")
	environment := r.URL.Query().Get("environment")
	agg := metricAggFor(info.Type, r.URL.Query().Get("agg"))
	matcher := metric.LabelMatcher{Key: r.URL.Query().Get("label_key"), Value: r.URL.Query().Get("label_value")}
	var matchers []metric.LabelMatcher
	if matcher.Key != "" {
		matchers = []metric.LabelMatcher{matcher}
	}

	from, now := tr.From, tr.To
	// metric_points читается сырым (без 5m-MV, как у perf) — шаг без выравнивания (align=0).
	step := autoStep(tr.Window(), time.Minute, 0, metricChartBuckets)
	points, err := h.Metrics.Series(r.Context(), projectID, name, environment, "", matchers, agg, from, now, step)
	if err != nil {
		h.renderMetricUnavailable(w, r, projectID, name, err)
		return
	}
	// пустые корзины помечаем NaN — линия рвётся на них, ось X идёт по всему интервалу.
	points = fillSeries(points, from, now, step,
		func(p metric.Point) time.Time { return p.T },
		func(t time.Time) metric.Point { return metric.Point{T: t, V: math.NaN()} })
	labels, err := h.Metrics.Labels(r.Context(), projectID, name, from, now)
	if err != nil {
		h.renderMetricUnavailable(w, r, projectID, name, err)
		return
	}
	environments, err := h.Metrics.Environments(r.Context(), projectID, name, from, now)
	if err != nil {
		h.renderMetricUnavailable(w, r, projectID, name, err)
		return
	}
	var deploys []deploy.Deployment
	if h.Deploy != nil {
		deploys, _ = h.Deploy.List(r.Context(), projectID, from, now, 20)
	}
	vm := templates.MetricDetailVM{
		ProjectID:    projectID,
		Info:         info,
		Range:        timeRangeVM(tr),
		Agg:          agg,
		Environment:  environment,
		Environments: environments,
		Labels:       labels,
		LabelKey:     matcher.Key,
		LabelValue:   matcher.Value,
		Chart:        metricSeriesSVG(r.Context(), points, info.Unit, h.metricThresholdsFor(r.Context(), projectID, name, agg), deploys, metricChartWidth, metricChartHeight),
		Percentiles:  info.Type == "histogram",
	}
	_ = templates.MetricDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) renderMetricUnavailable(w http.ResponseWriter, r *http.Request, projectID int64, name string, err error) {
	slog.Warn("metrics: detail failed", "project_id", projectID, "metric", name, "err", err)
	vm := templates.MetricDetailVM{
		ProjectID:  projectID,
		Info:       metric.MetricInfo{Name: name},
		LoadFailed: true,
	}
	_ = templates.MetricDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) metricThresholdsFor(ctx context.Context, projectID int64, name, agg string) []metricThreshold {
	if h.MetricRules == nil {
		return nil
	}
	rules, err := h.MetricRules.List(ctx, projectID)
	if err != nil {
		return nil
	}
	var out []metricThreshold
	for _, rule := range rules {
		if rule.Enabled && rule.MetricName == name && rule.Aggregation == agg {
			out = append(out, metricThreshold{Value: rule.Threshold, Comparator: rule.Comparator})
		}
	}
	return out
}

func metricAggFor(typ, agg string) string {
	if typ == "histogram" {
		switch agg {
		case "p50", "p95", "p99", "avg":
			return agg
		default:
			return "p95"
		}
	}
	switch agg {
	case "max", "min", "sum", "avg":
		return agg
	default:
		return "avg"
	}
}

func metricDetailURL(projectID int64, name string) string {
	return metricsPath(projectID) + "/" + url.PathEscape(name)
}
