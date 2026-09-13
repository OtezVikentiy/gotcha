package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func profileRegressionsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/profile-regressions"
}

func profileRegressionStatusFilter(v string) string {
	switch v {
	case "resolved":
		return "resolved"
	case "all":
		return "all"
	default:
		return "open"
	}
}

func (h *Handler) profileRegressionsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.ProfileRegressions == nil {
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
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	filter := profileRegressionStatusFilter(r.URL.Query().Get("status"))
	regs, err := h.ProfileRegressions.List(r.Context(), projectID, filter, 200)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	ackedByIDs := make([]int64, 0, len(regs))
	for _, reg := range regs {
		if reg.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *reg.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	_ = templates.ProfileRegressionsList(projectID, regs, filter, h.currentEmail(r), canOperate, ackedBy).Render(r.Context(), w)
}
