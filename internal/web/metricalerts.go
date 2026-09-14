package web

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func metricAlertsPath(projectID int64) string {
	return metricsPath(projectID) + "/alerts"
}

func (h *Handler) metricAlertsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.MetricRules == nil || h.MetricIncidents == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderMetricAlerts(w, r, http.StatusOK, projectID, nil, "")
}

func metricRuleFormState(r *http.Request) templates.FormState {
	f := templates.FormState{}
	for _, name := range []string{
		"metric_name", "aggregation", "comparator", "threshold",
		"window_seconds", "environment", "label_key", "label_value", "severity",
	} {
		if v := r.FormValue(name); v != "" {
			f[name] = v
		}
	}
	if metricRuleEnabled(r) {
		f["enabled"] = "on"
	}
	return f
}

func (h *Handler) renderMetricAlerts(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	rules, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	incidents, err := h.MetricIncidents.List(r.Context(), projectID, 100)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	ackedByIDs := make([]int64, 0, len(incidents))
	for _, in := range incidents {
		if in.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *in.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	var known []string
	if h.Metrics != nil {
		if infos, err := h.Metrics.ListMetrics(r.Context(), projectID, ""); err == nil {
			known = make([]string, 0, len(infos))
			for _, mi := range infos {
				known = append(known, mi.Name)
			}
		} else {
			slog.Warn("metric alerts: cannot list known metric names", "project_id", projectID, "error", err)
		}
	}
	w.WriteHeader(status)
	_ = templates.MetricAlerts(projectID, rules, incidents, known, form, errMsg, h.currentEmail(r), ackedBy).Render(r.Context(), w)
}

func (h *Handler) metricAlertCreate(w http.ResponseWriter, r *http.Request) {
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
	if h.MetricRules == nil || h.MetricIncidents == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	rule, errKey := metricRuleFromForm(r, projectID)
	if errKey != "" {
		form := metricRuleFormState(r).Open(templates.MetricRuleCreateModalID)
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, form, i18n.T(r.Context(), errKey))
		return
	}
	if _, err := h.MetricRules.Create(r.Context(), rule); err != nil {
		if errors.Is(err, metric.ErrInvalidRule) {
			form := metricRuleFormState(r).Open(templates.MetricRuleCreateModalID)
			h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, form, i18n.T(r.Context(), "err.metricalert.invalid_rule"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}

// второй результат — i18n-ключ ошибки валидации ("" — успех).
func metricRuleFromForm(r *http.Request, projectID int64) (metric.Rule, string) {
	threshold, err := strconv.ParseFloat(r.FormValue("threshold"), 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		// ParseFloat принимает "NaN"/"Inf" без ошибки — такой порог сломал бы
		// сравнение и график (y="NaN"), отклоняем явно.
		return metric.Rule{}, "err.metricalert.threshold_finite"
	}
	window, err := strconv.Atoi(r.FormValue("window_seconds"))
	if err != nil || window <= 0 {
		return metric.Rule{}, "err.metricalert.window_positive"
	}
	// select формы отдаёт только "" | critical | warning, но проверяем и прямой
	// POST мимо формы — иначе произвольная строка дойдёт до CHECK-ограничения БД.
	severity := r.FormValue("severity")
	if severity != "" && severity != escalation.SeverityCritical && severity != escalation.SeverityWarning {
		return metric.Rule{}, "err.metricalert.invalid_rule"
	}
	return metric.Rule{
		ProjectID:     projectID,
		MetricName:    r.FormValue("metric_name"),
		Aggregation:   r.FormValue("aggregation"),
		Comparator:    r.FormValue("comparator"),
		Threshold:     threshold,
		WindowSeconds: window,
		Environment:   r.FormValue("environment"),
		LabelKey:      r.FormValue("label_key"),
		LabelValue:    r.FormValue("label_value"),
		Enabled:       metricRuleEnabled(r),
		Severity:      severity,
	}, ""
}

// hidden "off" перед чекбоксом даёт r.Form["enabled"]=["off"] у снятого и ["off","on"]
// у взведённого; полное отсутствие поля (POST мимо формы) — включено, тот же дефолт и при правке.
func metricRuleEnabled(r *http.Request) bool {
	vals, ok := r.Form["enabled"]
	if !ok {
		return true
	}
	for _, v := range vals {
		if v == "on" {
			return true
		}
	}
	return false
}

// открытый инцидент правила не трогаем — оценщик сам перечитывает условие на каждом тике.
func (h *Handler) metricAlertUpdate(w http.ResponseWriter, r *http.Request) {
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
	if h.MetricRules == nil || h.MetricIncidents == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	ruleID, err := strconv.ParseInt(r.PathValue("ruleID"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	rule, errKey := metricRuleFromForm(r, projectID)
	if errKey != "" {
		form := metricRuleFormState(r).Open(templates.EditMetricRuleModalID(ruleID))
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, form, i18n.T(r.Context(), errKey))
		return
	}
	rule.ID = ruleID
	if _, err := h.MetricRules.Update(r.Context(), rule); err != nil {
		if errors.Is(err, metric.ErrRuleNotFound) {
			h.notFound(w, r)
			return
		}
		if errors.Is(err, metric.ErrInvalidRule) {
			form := metricRuleFormState(r).Open(templates.EditMetricRuleModalID(ruleID))
			h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, form, i18n.T(r.Context(), "err.metricalert.invalid_rule"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) metricAlertDelete(w http.ResponseWriter, r *http.Request) {
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
	if h.MetricRules == nil || h.MetricIncidents == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	ruleID, err := strconv.ParseInt(r.FormValue("rule_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	// CSP без unsafe-inline не исполняет inline confirm() — подтверждение отдельной страницей.
	if r.FormValue("confirmed") != "yes" {
		name := ""
		if rule, ok, err := h.MetricRules.Get(r.Context(), ruleID); err == nil && ok && rule.ProjectID == projectID {
			name = rule.MetricName + " " + comparatorSymbol(rule.Comparator) + " " + humanize.CompactNumber(rule.Threshold)
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.metric_rule_delete.message", "confirm.delete",
			metricAlertsPath(projectID), metricAlertsPath(projectID)+"/delete",
			[]templates.HiddenField{{Name: "rule_id", Value: strconv.FormatInt(ruleID, 10)}},
			"name", name)
		return
	}
	if err := h.MetricRules.Delete(r.Context(), ruleID, projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}
