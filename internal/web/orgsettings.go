package web

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
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

func orgSettingsQuotaPath(orgID int64) string {
	return orgSettingsPath(orgID) + "/quota"
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
	case errors.Is(err, org.ErrInvalidQuota):
		return "квота не может быть отрицательной"
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

// requireOrgOwner — SSO-настройки доступны только владельцу орга (более узкая
// граница, чем requireOrgRole owner/admin): SSO — доверенная точка входа. Не
// owner → 404 (как прочие owner-only действия). Возвращает ok.
func (h *Handler) requireOrgOwner(w http.ResponseWriter, r *http.Request, orgID, uid int64) bool {
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil || role != org.RoleOwner {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
		return false
	}
	return true
}

// orgSettingsSSO — POST /orgs/{id}/settings/sso: owner настраивает per-org OIDC.
func (h *Handler) orgSettingsSSO(w http.ResponseWriter, r *http.Request) {
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
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := org.SSOConfig{
		OrgID:        orgID,
		Issuer:       r.FormValue("issuer"),
		ClientID:     r.FormValue("client_id"),
		ClientSecret: r.FormValue("client_secret"),
		Domain:       r.FormValue("domain"),
		DefaultRole:  r.FormValue("default_role"),
		Enforced:     r.FormValue("enforced") != "",
	}
	switch err := h.Org.UpsertSSO(r.Context(), cfg); {
	case err == nil:
		http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
	case errors.Is(err, org.ErrDomainTaken):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "домен уже используется другой организацией", "")
	case errors.Is(err, org.ErrInvalidSSO) || errors.Is(err, org.ErrInvalidRole):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "заполните issuer, client_id, client_secret и домен", "")
	default:
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
	}
}

// orgSettingsSSODelete — POST /orgs/{id}/settings/sso/delete: owner убирает SSO.
func (h *Handler) orgSettingsSSODelete(w http.ResponseWriter, r *http.Request) {
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
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := h.Org.DeleteSSO(r.Context(), orgID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
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
	// usage — счётчик событий организации ЗА ТЕКУЩИЙ месяц (org_usage
	// ключуется по (org_id, period_month)); блок «Квота» показывает его рядом
	// с лимитом (o.EventQuota, уже загружен в Get выше).
	usage, err := h.Org.Usage(r.Context(), orgID, time.Now())
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(status)
	_ = templates.OrgSettings(o, members, uid, usage, errMsg, inviteLink, h.ssoSettingsVM(r, orgID, uid), h.currentEmail(r)).Render(r.Context(), w)
}

// ssoSettingsVM собирает данные секции SSO настроек орга (этап 10). Секция
// видна только owner'у; client_secret обратно не отдаём (показываем «настроено»).
func (h *Handler) ssoSettingsVM(r *http.Request, orgID, uid int64) templates.SSOSettings {
	vm := templates.SSOSettings{
		RedirectURI: h.BaseURL + "/auth/oauth/" + ssoProviderPrefix + strconv.FormatInt(orgID, 10) + "/callback",
	}
	if role, err := h.Org.Role(r.Context(), orgID, uid); err == nil && role == org.RoleOwner {
		vm.IsOwner = true
	}
	if cfg, ok, err := h.Org.SSOByOrg(r.Context(), orgID); err == nil && ok {
		vm.Configured = true
		vm.Issuer = cfg.Issuer
		vm.ClientID = cfg.ClientID
		vm.Domain = cfg.Domain
		vm.DefaultRole = cfg.DefaultRole
		vm.Enforced = cfg.Enforced
	}
	return vm
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
	inviteLink := h.BaseURL + inviteAcceptPath(token)

	// Упрощение (план 6, задача 5): полноценный outbox (internal/notify)
	// привязан к channel_id NOT NULL — он существует для алертов конкретного
	// проекта, а приглашение — организационное событие без проекта/канала.
	// Поэтому письмо шлётся СИНХРОННО напрямую через notify.EmailSender,
	// best-effort: ошибка SMTP не должна ронять сам POST — ссылка-приглашение
	// всё равно показывается в UI ниже и её можно передать вручную.
	if h.Email != nil && h.Email.Configured() {
		payload := map[string]any{
			"subject": "Приглашение в организацию Gotcha",
			"body":    "Вас пригласили в организацию Gotcha. Ссылка для принятия приглашения: " + inviteLink,
		}
		if err := h.Email.Send(r.Context(), notify.Target{Kind: "email", Target: email}, payload); err != nil {
			slog.Warn("orgSettingsInvite: failed to send invite email", "email", email, "org_id", orgID, "err", err)
		}
	}

	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", inviteLink)
}

// orgSettingsQuota — POST /orgs/{id}/settings/quota: quota (событий в
// месяц). Доступ только owner/admin (requireOrgRole — та же граница, что и у
// остальных настроек организации). Отрицательное значение (или нечисловое
// поле) → 422 (ErrInvalidQuota); 0 = безлимит (org.SetQuota).
func (h *Handler) orgSettingsQuota(w http.ResponseWriter, r *http.Request) {
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
	quota, err := strconv.ParseInt(r.FormValue("quota"), 10, 64)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(org.ErrInvalidQuota), "")
		return
	}
	if err := h.Org.SetQuota(r.Context(), orgID, quota); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(err), "")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
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
