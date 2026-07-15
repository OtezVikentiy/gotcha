package web

import (
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func metricAlertsPath(projectID int64) string {
	return metricsPath(projectID) + "/alerts"
}

// metricAlertsPage — GET /projects/{id}/metrics/alerts: форма создания правила,
// список правил и инцидентов. Доступ owner/admin (requireProjectRole).
func (h *Handler) metricAlertsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil || h.MetricIncidents == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	h.renderMetricAlerts(w, r, http.StatusOK, projectID, "")
}

func (h *Handler) renderMetricAlerts(w http.ResponseWriter, r *http.Request, status int, projectID int64, errMsg string) {
	rules, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	incidents, err := h.MetricIncidents.List(r.Context(), projectID, 100)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(status)
	_ = templates.MetricAlerts(projectID, rules, incidents, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// metricAlertCreate — POST /projects/{id}/metrics/alerts: создать правило.
func (h *Handler) metricAlertCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil || h.MetricIncidents == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	threshold, err := strconv.ParseFloat(r.FormValue("threshold"), 64)
	if err != nil {
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, "порог должен быть числом")
		return
	}
	window, err := strconv.Atoi(r.FormValue("window_seconds"))
	if err != nil || window <= 0 {
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, "окно должно быть положительным числом секунд")
		return
	}
	rule := metric.Rule{
		ProjectID:     projectID,
		MetricName:    r.FormValue("metric_name"),
		Aggregation:   r.FormValue("aggregation"),
		Comparator:    r.FormValue("comparator"),
		Threshold:     threshold,
		WindowSeconds: window,
		Environment:   r.FormValue("environment"),
		LabelKey:      r.FormValue("label_key"),
		LabelValue:    r.FormValue("label_value"),
		Enabled:       true,
	}
	if _, err := h.MetricRules.Create(r.Context(), rule); err != nil {
		if errors.Is(err, metric.ErrInvalidRule) {
			h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, "неверные параметры правила")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}

// metricAlertDelete — POST /projects/{id}/metrics/alerts/delete: удалить правило.
func (h *Handler) metricAlertDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil || h.MetricIncidents == nil {
		http.NotFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ruleID, err := strconv.ParseInt(r.FormValue("rule_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad rule id", http.StatusBadRequest)
		return
	}
	if err := h.MetricRules.Delete(r.Context(), ruleID, projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}
