package web

import (
	"context"
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

// metricPeriodWindow — окно и шаг корзины для query-параметра period. В отличие
// от perf-страниц, метрики читаются из сырой metric_points (без 5m MV), поэтому
// шаг может быть мельче.
func metricPeriodWindow(period string) (window, step time.Duration, name string) {
	switch period {
	case "1h":
		return time.Hour, time.Minute, "1h"
	case "7d":
		return 7 * 24 * time.Hour, time.Hour, "7d"
	default:
		return 24 * time.Hour, 10 * time.Minute, "24h"
	}
}

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

	// Тип метрики: перцентили допустимы только для histogram.
	metrics, err := h.Metrics.ListMetrics(r.Context(), projectID, "")
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	var info metric.MetricInfo
	found := false
	for _, m := range metrics {
		if m.Name == name {
			info, found = m, true
			break
		}
	}
	if !found {
		h.notFound(w, r)
		return
	}

	window, step, period := metricPeriodWindow(r.URL.Query().Get("period"))
	environment := r.URL.Query().Get("environment")
	agg := metricAggFor(info.Type, r.URL.Query().Get("agg"))
	matcher := metric.LabelMatcher{Key: r.URL.Query().Get("label_key"), Value: r.URL.Query().Get("label_value")}

	now := time.Now().UTC()
	from := now.Add(-window)
	points, err := h.Metrics.Series(r.Context(), projectID, name, environment, matcher, agg, from, now, step)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	labels, err := h.Metrics.Labels(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	environments, err := h.Metrics.Environments(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	vm := templates.MetricDetailVM{
		ProjectID:    projectID,
		Info:         info,
		Period:       period,
		Agg:          agg,
		Environment:  environment,
		Environments: environments,
		Labels:       labels,
		LabelKey:     matcher.Key,
		LabelValue:   matcher.Value,
		Chart:        metricSeriesSVG(points, info.Unit, h.metricThresholdsFor(r.Context(), projectID, name, agg), 720, 200),
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
