package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// incidentAck — POST /projects/{id}/incidents/{source}/{incident_id}/ack:
// единый ack-эндпоинт пяти источников инцидентов (B4, T10). Диспатч по
// {source} на Acknowledge нужного стора — сигнатуры сторов зеркальны
// (incidentID, projectID, userID), project_id в WHERE каждого — defense-in-
// depth (см. host.IncidentService.Acknowledge и его аналоги): оператор
// проекта A не подтвердит инцидент проекта B подобранным id, даже если этот
// хендлер когда-нибудь ошибётся в маршрутизации.
//
// Доступ — оператор проекта (requireProjectOperator), как у остальных
// мутаций инцидентов/окон/правил. Успех и «уже подтверждён» (ok=false —
// идемпотентно, не ошибка) ведут туда же: на Referer (safeRedirect), если он
// same-origin, иначе на список инцидентов проекта — человек возвращается на
// тот же экран, с которого нажал кнопку.
func (h *Handler) incidentAck(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	source := r.PathValue("source")
	incidentID, err := strconv.ParseInt(r.PathValue("incident_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad incident id", http.StatusBadRequest)
		return
	}

	// acked=false — инцидент уже подтверждён, закрыт, или incidentID/project_id
	// не совпали (кросс-тенант): идемпотентно, не ошибка — та же кнопка
	// «Подтвердить», повторно отправленная (двойной клик, вкладка в фоне), не
	// должна показывать человеку сбой. Значение никуда дальше не идёт — редирект
	// один и тот же в обоих случаях.
	switch source {
	case "host":
		if h.HostIncidents == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.HostIncidents.Acknowledge(r.Context(), incidentID, projectID, uid)
	case "metric":
		if h.MetricIncidents == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.MetricIncidents.Acknowledge(r.Context(), incidentID, projectID, uid)
	case "trace":
		if h.Regressions == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.Regressions.Acknowledge(r.Context(), incidentID, projectID, uid)
	case "profile":
		if h.ProfileRegressions == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.ProfileRegressions.Acknowledge(r.Context(), incidentID, projectID, uid)
	case "slo":
		if h.SLO == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.SLO.Acknowledge(r.Context(), incidentID, projectID, uid)
	default:
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}
