package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// orgProjectsPath — путь до списка проектов организации: цель редиректа
// GET /projects (projectsRedirect ниже) и топбарного селекта организации
// (whereAmI, layout.templ).
func orgProjectsPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects"
}

// orgProjectsPage — GET /orgs/{id}/projects: плоский список проектов ОДНОЙ
// организации — дверь для селекта организации в топбаре (задача 4 nav-ia) и
// дом для создания проекта. Уровень lvlUser с фильтрацией здесь: страницу
// видит любой участник организации, кнопку создания — только owner/admin.
// Невалидный id и организация, в которой юзер не состоит, отвечают ОДИНАКОВО
// — 404: адрес не должен подтверждать существование чужой организации (тот
// же принцип, что и у requireOrgRole).
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
	// Кнопка/модалка создания — только owner/admin, тот же принцип, что и у
	// canCreate в renderProjectsList: одна организация в списке — эта.
	var canCreate []templates.OrgOption
	if role == org.RoleOwner || role == org.RoleAdmin {
		canCreate = []templates.OrgOption{{ID: o.ID, Name: o.Name}}
	}
	w.WriteHeader(http.StatusOK)
	_ = templates.OrgProjects(o, projects, canCreate, h.currentEmail(r)).Render(r.Context(), w)
}

// projectsRedirect — GET /projects: раньше рендерил плоский список всех
// проектов пользователя (эта таблица теперь живёт на 422 POST /projects/new,
// см. renderProjectsList), теперь — просто дверь в него, ведущая на список
// проектов ОДНОЙ организации. Целевая организация — запомненная (проект из
// projCookie, если юзер всё ещё в его организации состоит), иначе первая по
// порядку OrgsOf. Без единой организации (например, у только что
// исключённого из последней организации участника) уводит на /onboarding —
// тот же тупик, что и у index() для юзера без организаций.
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
