package web

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func metricsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/metrics"
}

// metricChartBuckets — целевое число корзин графика метрики. Шаг подбирается
// autoStep по окну (не мельче минуты): 1ч→~1м, 24ч→~12м, 7д→~1.4ч, 30д→~6ч.
const metricChartBuckets = 120

// metricsList — GET /projects/{id}/metrics: перечень метрик проекта.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	environment := r.URL.Query().Get("environment")
	metrics, err := h.Metrics.ListMetrics(r.Context(), projectID, environment)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	_ = templates.MetricsList(projectID, metrics, environment, h.currentEmail(r)).Render(r.Context(), w)
}

// metricDetail — GET /projects/{id}/metrics/{name}: график ряда метрики.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	name := r.PathValue("name")

	// Тип метрики: перцентили допустимы только для histogram. Точечный поиск по
	// (project_id, name) вместо скана всех метрик проекта ради одной.
	info, found, err := h.Metrics.MetricInfoByName(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}

	tr := parseTimeRange(r.URL.Query(), "24h")
	environment := r.URL.Query().Get("environment")
	agg := metricAggFor(info.Type, r.URL.Query().Get("agg"))
	matcher := metric.LabelMatcher{Key: r.URL.Query().Get("label_key"), Value: r.URL.Query().Get("label_value")}

	from, now := tr.From, tr.To
	// Метрики читают сырую metric_points (без 5m-MV, как у perf), поэтому шаг
	// может быть мельче — не мельче минуты, без выравнивания (align=0).
	step := autoStep(tr.Window(), time.Minute, 0, metricChartBuckets)
	points, err := h.Metrics.Series(r.Context(), projectID, name, environment, matcher, agg, from, now, step)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Дозаполняем окно: пустые корзины помечаем NaN — линия рвётся на них, а
	// ось X идёт по всему выбранному интервалу (а не по диапазону с данными).
	points = fillSeries(points, from, now, step,
		func(p metric.Point) time.Time { return p.T },
		func(t time.Time) metric.Point { return metric.Point{T: t, V: math.NaN()} })
	labels, err := h.Metrics.Labels(r.Context(), projectID, name, from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	environments, err := h.Metrics.Environments(r.Context(), projectID, name, from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
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
		Chart:        metricSeriesSVG(r.Context(), points, info.Unit, h.metricThresholdsFor(r.Context(), projectID, name, agg), 720, 200),
		Percentiles:  info.Type == "histogram",
	}
	_ = templates.MetricDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

// metricThresholdsFor собирает пороги включённых правил алертов для этой
// метрики (совпадающих по имени и агрегации) — для отрисовки пороговых линий на
// графике. Ошибка загрузки не критична: график просто рисуется без линий.
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

// metricAggFor нормализует агрегацию под тип метрики: перцентили только для
// histogram; иначе дефолт avg. Скалярные допускают avg/max/min/sum.
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

// metricDetailURL строит ссылку на страницу метрики с экранированным именем.
func metricDetailURL(projectID int64, name string) string {
	return metricsPath(projectID) + "/" + url.PathEscape(name)
}
