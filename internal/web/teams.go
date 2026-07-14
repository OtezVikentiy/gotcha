package web

import (
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func orgTeamsPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/teams"
}

func teamMembersPath(teamID int64) string {
	return "/teams/" + strconv.FormatInt(teamID, 10) + "/members"
}

func teamMembersRemovePath(teamID int64) string {
	return teamMembersPath(teamID) + "/remove"
}

func teamProjectsPath(teamID int64) string {
	return "/teams/" + strconv.FormatInt(teamID, 10) + "/projects"
}

func teamProjectsDetachPath(teamID int64) string {
	return teamProjectsPath(teamID) + "/detach"
}

// errCrossOrgProject — попытка привязать к команде проект, не принадлежащий
// организации этой команды.
var errCrossOrgProject = errors.New("web: project belongs to a different organization")

// parsePathTeamID достаёт teamID из {id} пути /teams/{id}*; на невалидный id —
// 404, тот же принцип, что и у parsePathOrgID/parsePathProjectID.
func parsePathTeamID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	teamID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return teamID, true
}

// requireTeamRole резолвит teamID -> orgID (org.TeamOrg) и проверяет роль
// вызывающего в этой организации (requireOrgRole): несуществующая команда и
// недостаточная роль дают одну и ту же стилизованную 404 — не палим
// существование чужой команды.
func (h *Handler) requireTeamRole(w http.ResponseWriter, r *http.Request, teamID, userID int64) (int64, bool) {
	orgID, err := h.Org.TeamOrg(r.Context(), teamID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return 0, false
	}
	if _, ok := h.requireOrgRole(w, r, orgID, userID); !ok {
		return 0, false
	}
	return orgID, true
}

func teamsErrorMessage(err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidSlug):
		return "slug должен состоять из строчных латинских букв, цифр и дефисов (1..64 символа)"
	case errors.Is(err, org.ErrSlugTaken):
		return "такой slug уже занят"
	case errors.Is(err, org.ErrNotMember):
		return "пользователь не является участником организации"
	case errors.Is(err, errCrossOrgProject):
		return "проект принадлежит другой организации"
	default:
		return "не удалось выполнить действие"
	}
}

// teamsPage — GET /orgs/{id}/teams: список команд организации. Доступ только
// owner/admin (requireOrgRole).
func (h *Handler) teamsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	h.renderTeamsPage(w, r, http.StatusOK, orgID, "")
}

// renderTeamsPage — общий рендер: используется и GET-обработчиком, и всеми
// POST-обработчиками этого файла на 422 (то же сообщение об ошибке на месте,
// без редиректа — тот же принцип, что и renderOrgSettings).
func (h *Handler) renderTeamsPage(w http.ResponseWriter, r *http.Request, status int, orgID int64, errMsg string) {
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	teams, err := h.Org.TeamsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	views := make([]templates.TeamView, len(teams))
	for i, tm := range teams {
		members, err := h.Org.TeamMembers(r.Context(), tm.ID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		projects, err := h.Org.TeamProjects(r.Context(), tm.ID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		views[i] = templates.TeamView{Team: tm, Members: members, Projects: projects}
	}
	orgMembers, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	orgProjects, err := h.Org.ProjectsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(status)
	_ = templates.Teams(o, views, orgMembers, orgProjects, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// teamsCreate — POST /orgs/{id}/teams: slug, name. ErrInvalidSlug/ErrSlugTaken
// → 422.
func (h *Handler) teamsCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug := r.FormValue("slug")
	name := r.FormValue("name")
	if _, err := h.Org.CreateTeam(r.Context(), orgID, slug, name); err != nil {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, teamsErrorMessage(err))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamMembersAdd — POST /teams/{id}/members: user_id. ErrNotMember (не
// участник организации команды) → 422.
func (h *Handler) teamMembersAdd(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if err := h.Org.AddTeamMember(r.Context(), teamID, targetID); err != nil {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, teamsErrorMessage(err))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamMembersRemove — POST /teams/{id}/members/remove: user_id.
func (h *Handler) teamMembersRemove(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if err := h.Org.RemoveTeamMember(r.Context(), teamID, targetID); err != nil {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, teamsErrorMessage(err))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamProjectsAttach — POST /teams/{id}/projects: project_id. Проект должен
// принадлежать той же организации, что и команда, иначе 422
// (errCrossOrgProject) — иначе можно было бы дать команде одной организации
// доступ к issues чужой.
func (h *Handler) teamProjectsAttach(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad project_id", http.StatusBadRequest)
		return
	}
	projectOrgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, teamsErrorMessage(errCrossOrgProject))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if projectOrgID != orgID {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, teamsErrorMessage(errCrossOrgProject))
		return
	}
	if err := h.Org.AttachTeam(r.Context(), projectID, teamID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamProjectsDetach — POST /teams/{id}/projects/detach: project_id.
// DetachTeam идемпотентен — здесь не нужна проверка org, потому что она
// только сужает то, к чему у команды и так есть доступ.
func (h *Handler) teamProjectsDetach(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad project_id", http.StatusBadRequest)
		return
	}
	if err := h.Org.DetachTeam(r.Context(), projectID, teamID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}
