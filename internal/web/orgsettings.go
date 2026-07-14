package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func orgSettingsPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings"
}

func orgSettingsRolePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/role"
}

func orgSettingsRemovePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/remove"
}

func orgSettingsInvitePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/invite"
}

func inviteAcceptPath(token string) string {
	return "/invite/" + token
}

// reInviteEmail — та же намеренно простая проверка формата, что и
// auth.reEmail (не экспортирован оттуда): один @, непустые локальная часть
// и домен, в домене есть точка.
var reInviteEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func validInviteEmail(email string) bool {
	return email != "" && len(email) <= 254 && reInviteEmail.MatchString(email)
}

// orgSettingsErrorMessage переводит доменные ошибки org.Service в
// человекочитаемое сообщение для 422-страницы настроек организации.
func orgSettingsErrorMessage(err error) string {
	switch {
	case errors.Is(err, org.ErrLastOwner):
		return "нельзя понизить или удалить последнего владельца организации"
	case errors.Is(err, org.ErrInvalidRole):
		return "недопустимая роль"
	case errors.Is(err, org.ErrNotMember):
		return "пользователь не является участником организации"
	case errors.Is(err, org.ErrOwnerOnly):
		return ownerLevelAccessMessage
	default:
		return "не удалось выполнить действие"
	}
}

const ownerLevelAccessMessage = "только владелец может управлять доступом уровня владельца"

// parsePathOrgID достаёт orgID из {id} пути /orgs/{id}/settings*; на
// невалидный id — 404 (тот же принцип, что и у числовых id issue/project:
// не палим существование записи форматом ответа).
func parsePathOrgID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	orgID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return orgID, true
}

// orgSettingsPage — GET /orgs/{id}/settings: таблица участников (email,
// роль, форма смены роли, форма удаления — не для себя) и форма приглашения.
// Доступ только owner/admin (requireOrgRole).
func (h *Handler) orgSettingsPage(w http.ResponseWriter, r *http.Request) {
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
	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", "")
}

// renderOrgSettings — общий рендер страницы настроек: используется и
// GET-обработчиком, и POST-обработчиками (422 с сообщением об ошибке на
// месте, без редиректа — как логин/онбординг). POST .../invite при успехе
// тоже рендерит эту же страницу напрямую (без редиректа): одноразовый токен
// приглашения нельзя протащить через query string или Location, поэтому
// ссылка-приглашение показывается один раз, сразу в теле ответа POST.
func (h *Handler) renderOrgSettings(w http.ResponseWriter, r *http.Request, status int, orgID, uid int64, errMsg, inviteLink string) {
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	members, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(status)
	_ = templates.OrgSettings(o, members, uid, errMsg, inviteLink, h.currentEmail(r)).Render(r.Context(), w)
}

// orgSettingsRole — POST /orgs/{id}/settings/role: user_id, role. Менять
// роль себе нельзя (422); org.SetRoleAs сам защищает последнего owner'а
// (ErrLastOwner → 422), проверяет допустимость роли (ErrInvalidRole → 422) и
// привилегию эскалации (ErrOwnerOnly → 422).
func (h *Handler) orgSettingsRole(w http.ResponseWriter, r *http.Request) {
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
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if targetID == uid {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "нельзя изменить собственную роль", "")
		return
	}
	role := org.Role(r.FormValue("role"))
	// SetRoleAs — актёрозависимый вариант (security fix): проверяет роль
	// актёра, роль цели и last-owner защиту в ОДНОЙ транзакции с самой
	// мутацией (см. её комментарий в internal/org/member.go), закрывая TOCTOU
	// между requireOrgRole и мутацией.
	if err := h.Org.SetRoleAs(r.Context(), orgID, uid, targetID, role); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(err), "")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsRemove — POST /orgs/{id}/settings/remove: user_id. Убрать себя
// нельзя (422); org.RemoveMemberAs сам защищает последнего owner'а
// (ErrLastOwner → 422) и привилегию эскалации (ErrOwnerOnly → 422).
func (h *Handler) orgSettingsRemove(w http.ResponseWriter, r *http.Request) {
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
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if targetID == uid {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "нельзя удалить себя из организации", "")
		return
	}
	// RemoveMemberAs — тот же TOCTOU-фикс, что и у SetRoleAs выше.
	if err := h.Org.RemoveMemberAs(r.Context(), orgID, uid, targetID); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(err), "")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsInvite — POST /orgs/{id}/settings/invite: email, role
// (admin|member). Успех рендерит ту же страницу настроек с готовой
// ссылкой-приглашением {BaseURL}/invite/{token} прямо в теле ответа, без
// редиректа (см. renderOrgSettings).
func (h *Handler) orgSettingsInvite(w http.ResponseWriter, r *http.Request) {
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
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if !validInviteEmail(email) {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "невалидный email", "")
		return
	}
	role := org.Role(r.FormValue("role"))
	token, err := h.Org.Invite(r.Context(), orgID, email, role)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(err), "")
		return
	}
	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", h.BaseURL+inviteAcceptPath(token))
}

// inviteAcceptPage — GET /invite/{token}: страница «принять приглашение».
// GET не трогает БД — токен одноразовый, тратить его на простой просмотр
// страницы нельзя; валидность проверяется только на POST.
func (h *Handler) inviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	_ = templates.InviteAccept(token, "", h.currentEmail(r)).Render(r.Context(), w)
}

// inviteAcceptSubmit — POST /invite/{token}: org.AcceptInvite; успех → 303 /,
// невалидный/истёкший/уже использованный токен (ErrInviteInvalid) → 422
// styled-страница.
func (h *Handler) inviteAcceptSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token := r.PathValue("token")
	if _, err := h.Org.AcceptInvite(r.Context(), token, uid); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.InviteAccept(token, "приглашение недействительно, истекло или уже использовано", h.currentEmail(r)).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
