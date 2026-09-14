package web

import (
	"context"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// project_id проверяется в WHERE каждого стора (defense-in-depth): оператор проекта A не
// подтвердит инцидент проекта B подобранным id, даже если хендлер ошибётся в маршрутизации.
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
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}

	// acked=false — уже подтверждён, закрыт, или project_id не совпал: идемпотентно, не ошибка —
	// повторный клик (двойной клик, вкладка в фоне) не должен показывать сбой.
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
	case "uptime":
		if h.Uptime == nil {
			h.notFound(w, r)
			return
		}
		_, err = h.Uptime.Acknowledge(r.Context(), incidentID, projectID, uid)
	default:
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}

// Батч на дедуплицированные id, не запрос на строку. Отсутствие email в карте (удалённый
// пользователь) — не ошибка, ackControl тогда рисует только время.
func (h *Handler) ackedByEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	seen := make(map[int64]struct{}, len(ids))
	uniq := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	return h.Auth.UserEmails(ctx, uniq)
}
