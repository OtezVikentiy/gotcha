package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
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

func teamDeletePath(teamID int64) string {
	return "/teams/" + strconv.FormatInt(teamID, 10) + "/delete"
}

// teamName — имя команды для текста подтверждения; пустая строка, если
// команда не нашлась (тогда подтверждение показывает вопрос без имени, а
// само действие всё равно упрётся в ErrNotFound).
func (h *Handler) teamName(ctx context.Context, orgID, teamID int64) string {
	teams, err := h.Org.TeamsOf(ctx, orgID)
	if err != nil {
		return ""
	}
	for _, t := range teams {
		if t.ID == teamID {
			return t.Name
		}
	}
	return ""
}

// errCrossOrgProject — попытка привязать к команде проект, не принадлежащий
// организации этой команды.
var errCrossOrgProject = errors.New("web: project belongs to a different organization")

// parsePathTeamID достаёт teamID из {id} пути /teams/{id}*; на невалидный id —
// 404, тот же принцип, что и у parsePathOrgID/parsePathProjectID.
func (h *Handler) parsePathTeamID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	teamID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
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
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, false
	}
	if _, ok := h.requireOrgRole(w, r, orgID, userID); !ok {
		return 0, false
	}
	return orgID, true
}

func teamsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidSlug):
		return i18n.T(ctx, "error.slug.invalid")
	case errors.Is(err, org.ErrSlugTaken):
		return i18n.T(ctx, "error.slug.taken")
	case errors.Is(err, org.ErrInvalidName):
		return i18n.T(ctx, "error.name.empty")
	case errors.Is(err, org.ErrNotMember):
		return i18n.T(ctx, "error.org.not_member")
	case errors.Is(err, errCrossOrgProject):
		return i18n.T(ctx, "error.team.cross_org_project")
	default:
		return i18n.T(ctx, "error.action_failed")
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
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	h.renderTeamsPage(w, r, http.StatusOK, orgID, nil, "")
}

// renderTeamsPage — общий рендер: используется и GET-обработчиком, и всеми
// POST-обработчиками этого файла на 422 (то же сообщение об ошибке на месте,
// без редиректа — тот же принцип, что и renderOrgSettings).
func (h *Handler) renderTeamsPage(w http.ResponseWriter, r *http.Request, status int, orgID int64, form templates.FormState, errMsg string) {
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	teams, err := h.Org.TeamsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Участники и проекты всех команд — двумя запросами на страницу, а не по
	// два на каждую команду (аудит 2026-09-04, K8-2); порядок строк — как у
	// TeamMembers/TeamProjects.
	teamIDs := make([]int64, len(teams))
	for i, tm := range teams {
		teamIDs[i] = tm.ID
	}
	membersByTeam, err := h.Org.TeamMembersOf(r.Context(), teamIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	projectsByTeam, err := h.Org.TeamProjectsOf(r.Context(), teamIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	views := make([]templates.TeamView, len(teams))
	for i, tm := range teams {
		views[i] = templates.TeamView{Team: tm, Members: membersByTeam[tm.ID], Projects: projectsByTeam[tm.ID]}
	}
	orgMembers, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	orgProjects, err := h.Org.ProjectsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	w.WriteHeader(status)
	_ = templates.Teams(o, views, orgMembers, orgProjects, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// teamsCreate — POST /orgs/{id}/teams: slug, name. ErrInvalidSlug/ErrSlugTaken
// → 422.
func (h *Handler) teamsCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	slug := r.FormValue("slug")
	name := r.FormValue("name")
	if _, err := h.Org.CreateTeam(r.Context(), orgID, slug, name); err != nil {
		// Состояние формы возвращает введённое и открывает модалку: без него
		// человек получал закрытую пустую форму и сообщение где-то на странице.
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID,
			templates.FormState{"slug": slug, "name": name}.Open("new-team"),
			teamsErrorMessage(r.Context(), err))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamRename — POST /teams/{id}/rename: name.
//
// У команд был только жизненный цикл «создать/удалить»: опечатку в названии или
// переезд отдела нельзя было отразить, а пересоздание команды теряло и её
// участников, и привязанные проекты. Slug остаётся прежним — он участвует в
// адресах и в выдаче прав.
func (h *Handler) teamRename(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	// Проверка роли идёт по организации команды — она же даёт orgID для
	// перерисовки страницы и служит скоупом для самого UPDATE.
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	name := r.FormValue("name")
	if err := h.Org.RenameTeam(r.Context(), orgID, teamID, name); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID,
			templates.FormState{"name": name}.Open(templates.EditTeamModalID(teamID)),
			teamsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamMembersAdd — POST /teams/{id}/members: user_id. ErrNotMember (не
// участник организации команды) → 422.
func (h *Handler) teamMembersAdd(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if err := h.Org.AddTeamMember(r.Context(), teamID, targetID); err != nil {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, nil, teamsErrorMessage(r.Context(), err))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamMembersRemove — POST /teams/{id}/members/remove: user_id.
func (h *Handler) teamMembersRemove(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.team_member_remove.message", "confirm.remove",
			orgTeamsPath(orgID), teamMembersRemovePath(teamID),
			[]templates.HiddenField{{Name: "user_id", Value: strconv.FormatInt(targetID, 10)}})
		return
	}
	if err := h.Org.RemoveTeamMember(r.Context(), teamID, targetID); err != nil {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, nil, teamsErrorMessage(r.Context(), err))
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
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	projectOrgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, nil, teamsErrorMessage(r.Context(), errCrossOrgProject))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if projectOrgID != orgID {
		h.renderTeamsPage(w, r, http.StatusUnprocessableEntity, orgID, nil, teamsErrorMessage(r.Context(), errCrossOrgProject))
		return
	}
	if err := h.Org.AttachTeam(r.Context(), projectID, teamID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamProjectsDetach — POST /teams/{id}/projects/detach: project_id.
// DetachTeam идемпотентен — здесь не нужна проверка org, потому что она
// только сужает то, к чему у команды и так есть доступ.
func (h *Handler) teamProjectsDetach(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	// Двухшаговое подтверждение (№61): отвязка мгновенно отбирает у всей
	// команды доступ к проекту — с именами обеих сторон, иначе вопрос без
	// деталей не страхует от промаха по соседней строке.
	if r.FormValue("confirmed") != "yes" {
		projectName := ""
		if p, err := h.Org.GetProject(r.Context(), projectID); err == nil {
			projectName = p.Name
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.team_project_detach.message",
			"confirm.remove", orgTeamsPath(orgID), teamProjectsDetachPath(teamID),
			[]templates.HiddenField{{Name: "project_id", Value: strconv.FormatInt(projectID, 10)}},
			"project", projectName, "team", h.teamName(r.Context(), orgID, teamID))
		return
	}
	if err := h.Org.DetachTeam(r.Context(), projectID, teamID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}

// teamDelete — POST /teams/{id}/delete: удаление команды (№26). Членства и
// привязки к проектам уходят вместе с ней (org.DeleteTeam) — участники
// теряют доступ к её проектам сразу, поэтому действие двухшаговое, как
// прочие необратимые.
func (h *Handler) teamDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	teamID, ok := h.parsePathTeamID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireTeamRole(w, r, teamID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.team_delete.message",
			"confirm.delete", orgTeamsPath(orgID), teamDeletePath(teamID), nil,
			"name", h.teamName(r.Context(), orgID, teamID))
		return
	}
	if err := h.Org.DeleteTeam(r.Context(), orgID, teamID); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.team_deleted", 0)
	http.Redirect(w, r, orgTeamsPath(orgID), http.StatusSeeOther)
}
