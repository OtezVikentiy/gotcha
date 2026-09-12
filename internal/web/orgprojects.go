package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func orgProjectsPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects"
}

func (h *Handler) orgProjectsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	h.renderOrgProjects(w, r, http.StatusOK, uid, orgID, nil, "")
}

func (h *Handler) renderOrgProjects(w http.ResponseWriter, r *http.Request, status int, uid, orgID int64, form templates.FormState, errMsg string) {
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	projects, err := h.Org.ProjectsForUserInOrg(r.Context(), uid, orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	var canCreate []templates.OrgOption
	if role == org.RoleOwner || role == org.RoleAdmin {
		canCreate = []templates.OrgOption{{ID: o.ID, Name: o.Name}}
	}
	w.WriteHeader(status)
	_ = templates.OrgProjects(o, projects, canCreate, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) projectsRedirect(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	var orgID int64
	if pid := projCookieID(r); pid != 0 {
		if oid, err := h.Org.ProjectOrg(r.Context(), pid); err == nil {
			if _, err := h.Org.Role(r.Context(), oid, uid); err == nil {
				orgID = oid
			}
		}
	}
	if orgID == 0 {
		orgs, err := h.Org.OrgsOf(r.Context(), uid)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if len(orgs) == 0 {
			http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
			return
		}
		orgID = orgs[0].ID
	}
	http.Redirect(w, r, orgProjectsPath(orgID), http.StatusSeeOther)
}
