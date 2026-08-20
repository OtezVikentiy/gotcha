package web

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func metricAlertsPath(projectID int64) string {
	return metricsPath(projectID) + "/alerts"
}

// metricAlertsPage — GET /projects/{id}/metrics/alerts: форма создания правила,
// список правил и инцидентов. Доступ — оператор проекта (requireProjectOperator):
// владелец/админ организации ИЛИ участник команды, прикреплённой к проекту
// (спека cld/plans/2026-08-08-access-model-rework.md) — та же граница, что у
// мутаций монитора, не owner/admin (requireProjectRole).
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

// metricRuleFormState — введённые значения формы правила, чтобы вернуть их
// пользователю при ошибке валидации.
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
	return f
}

// renderMetricAlerts отрисовывает страницу. form — введённые пользователем
// значения: при ошибке валидации они возвращаются в форму, а сама модалка
// открывается с сервера. Раньше страница перерисовывалась без них, модалка
// закрывалась, и человек, ошибившийся в одном поле из семи, начинал сначала.
func (h *Handler) renderMetricAlerts(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	rules, err := h.MetricRules.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	incidents, err := h.MetricIncidents.List(r.Context(), projectID, 100)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Имена уже приходивших метрик — для выбора в форме вместо ввода руками.
	// Опечатка в свободном поле создавала правило, которое выглядит рабочим и не
	// срабатывает никогда: молчаливый отказ, о котором узнаёшь во время инцидента.
	// Ошибка чтения списка не должна ронять страницу — тогда просто нет подсказок.
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
	_ = templates.MetricAlerts(projectID, rules, incidents, known, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// metricAlertCreate — POST /projects/{id}/metrics/alerts: создать правило.
// Доступ — оператор проекта (requireProjectOperator), не только owner/admin.
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	threshold, err := strconv.ParseFloat(r.FormValue("threshold"), 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		// ParseFloat принимает "NaN"/"Inf" без ошибки; такой порог сломал бы
		// сравнение (алерт никогда не сработает) и график (y="NaN") — отклоняем.
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, metricRuleFormState(r), i18n.T(r.Context(), "err.metricalert.threshold_finite"))
		return
	}
	window, err := strconv.Atoi(r.FormValue("window_seconds"))
	if err != nil || window <= 0 {
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, metricRuleFormState(r), i18n.T(r.Context(), "err.metricalert.window_positive"))
		return
	}
	// severity — "" (наследовать дефолт источника 'warning') | critical | warning
	// (escalation.SeverityCritical/Warning, T4/T7 резолвят лесенку эскалации по
	// этому override — см. ruleSeverity в evaluator.go). Select в форме отдаёт
	// только эти три значения, но проверяем и на прямой POST мимо формы — иначе
	// произвольная строка дойдёт до CHECK-ограничения БД и 500-нет вместо 422.
	severity := r.FormValue("severity")
	if severity != "" && severity != escalation.SeverityCritical && severity != escalation.SeverityWarning {
		h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, metricRuleFormState(r), i18n.T(r.Context(), "err.metricalert.invalid_rule"))
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
		Severity:      severity,
	}
	if _, err := h.MetricRules.Create(r.Context(), rule); err != nil {
		if errors.Is(err, metric.ErrInvalidRule) {
			h.renderMetricAlerts(w, r, http.StatusUnprocessableEntity, projectID, metricRuleFormState(r), i18n.T(r.Context(), "err.metricalert.invalid_rule"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}

// metricAlertDelete — POST /projects/{id}/metrics/alerts/delete: удалить
// правило. Доступ — оператор проекта (requireProjectOperator), не только
// owner/admin.
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ruleID, err := strconv.ParseInt(r.FormValue("rule_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad rule id", http.StatusBadRequest)
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.metric_rule_delete.message", "confirm.delete",
			metricAlertsPath(projectID), metricAlertsPath(projectID)+"/delete",
			[]templates.HiddenField{{Name: "rule_id", Value: strconv.FormatInt(ruleID, 10)}})
		return
	}
	if err := h.MetricRules.Delete(r.Context(), ruleID, projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, metricAlertsPath(projectID), http.StatusSeeOther)
}
