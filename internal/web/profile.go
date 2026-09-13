package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Гейт единственного админа инстанса не срабатывает, если пользователь на инстансе
// один: запирать некого, следующий зарегистрировавшийся сам станет администратором.
func (h *Handler) profileDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Org != nil {
		owned, err := h.Org.SoleOwnedOrgNames(r.Context(), uid)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if len(owned) > 0 {
			h.renderError(w, r, http.StatusConflict,
				i18n.Tf(r.Context(), "profile.danger.delete_account.sole_owner", "orgs", strings.Join(owned, ", ")))
			return
		}
	}
	if !h.parseForm(w, r) {
		return
	}
	// Без confirmed=yes — страница подтверждения вместо удаления.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.account_delete.message",
			"profile.danger.delete_account.button", "/profile", "/profile/delete", nil)
		return
	}
	// Адрес читаем ДО удаления: после DeleteSelfAccount строки users уже нет, и очистка
	// приглашений ниже молча не выполнилась бы (TestProfileDeletePurgesPendingInvites ловит регресс).
	var email string
	if h.Org != nil {
		email = h.currentEmail(r)
	}
	// Гейт проверяется атомарно внутри DeleteSelfAccount — сессию рвём только после
	// успеха, иначе заблокированный гейтом админ терял бы сессию без удаления.
	if err := h.Auth.DeleteSelfAccount(r.Context(), uid); err != nil {
		if errors.Is(err, auth.ErrInstanceAdminBlocked) {
			h.renderError(w, r, http.StatusConflict, i18n.T(r.Context(), "profile.danger.delete_account.instance_admin"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if token, ok := auth.ReadSessionToken(r, h.Secure); ok {
		_ = h.Auth.DestroySession(r.Context(), token)
	}
	// Pending-инвайты не связаны с users по FK — каскад их не трогает, чистим отдельно, best-effort.
	if h.Org != nil {
		if email != "" {
			if _, err := h.Org.DeleteInvitesByEmail(r.Context(), email); err != nil {
				slog.Error("profileDelete: purge pending invites", "error", err)
			}
		} else {
			// currentEmail превращает в "" любую ошибку чтения, не только «юзера нет» — залогировать обязательно.
			slog.Error("profileDelete: could not read user email, pending invites not purged", "user_id", uid)
		}
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) profilePage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.renderProfile(w, r, http.StatusOK, uid, "", "")
}

func (h *Handler) renderProfile(w http.ResponseWriter, r *http.Request, status int, uid int64, errMsg, message string) {
	email, err := h.Auth.UserEmail(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	hasPassword, err := h.Auth.HasPassword(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	ids, err := h.Auth.ListIdentities(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	isInstanceAdmin, err := h.Auth.UserIsInstanceAdmin(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	linked := make([]templates.LinkedIdentity, 0, len(ids))
	linkedNames := make(map[string]bool, len(ids))
	for _, id := range ids {
		linkedNames[id.Provider] = true
		// Отвязать можно, если есть пароль ИЛИ это не последний способ входа.
		canUnlink := hasPassword || len(ids) > 1
		linked = append(linked, templates.LinkedIdentity{
			Provider:    id.Provider,
			DisplayName: h.providerDisplayName(r.Context(), id.Provider),
			Email:       id.Email,
			CanUnlink:   canUnlink,
		})
	}
	var linkable []templates.LinkableProvider
	if h.OAuth != nil {
		for _, p := range h.OAuth.List() {
			if !linkedNames[p.Name()] {
				linkable = append(linkable, templates.LinkableProvider{Name: p.Name(), DisplayName: providerLabel(r.Context(), p.Name(), p.DisplayName())})
			}
		}
	}
	w.WriteHeader(status)
	_ = templates.Profile(email, errMsg, message, hasPassword, linked, linkable, isInstanceAdmin, h.currentEmail(r)).Render(r.Context(), w)
}

// Fallback — сам ключ: провайдер мог быть выключен после привязки.
func (h *Handler) providerDisplayName(ctx context.Context, name string) string {
	if h.OAuth != nil {
		if p, ok := h.OAuth.Get(name); ok {
			return providerLabel(ctx, name, p.DisplayName())
		}
	}
	return name
}

func (h *Handler) profileIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	provider := r.FormValue("provider")

	hasPassword, err := h.Auth.HasPassword(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	ids, err := h.Auth.ListIdentities(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	// Без пароля и с единственной привязкой юзер лишился бы всякого доступа.
	if !hasPassword && len(ids) <= 1 {
		h.renderProfile(w, r, http.StatusConflict, uid,
			i18n.T(r.Context(), "err.profile.last_login_method"), "")
		return
	}
	switch err := h.Auth.UnlinkIdentity(r.Context(), uid, provider); {
	case err == nil:
		h.renderProfile(w, r, http.StatusOK, uid, "", i18n.T(r.Context(), "msg.profile.provider_unlinked"))
	case errors.Is(err, auth.ErrNoIdentity):
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, i18n.T(r.Context(), "err.profile.provider_not_linked"), "")
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
	}
}

// Успех не трогает сессии (в отличие от смены пароля) — юзер продолжает работать.
func (h *Handler) profilePasswordSet(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !h.loginLimiter.Allow("pwset|" + strconv.FormatInt(uid, 10)) {
		h.renderProfile(w, r, http.StatusTooManyRequests, uid, i18n.T(r.Context(), "err.auth.rate_limited"), "")
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	newPassword := r.FormValue("new")
	newPassword2 := r.FormValue("new2")
	if newPassword != newPassword2 {
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, i18n.T(r.Context(), "err.auth.passwords_differ"), "")
		return
	}
	switch err := h.Auth.SetPassword(r.Context(), uid, newPassword); {
	case err == nil:
		h.renderProfile(w, r, http.StatusOK, uid, "", i18n.T(r.Context(), "msg.profile.password_set"))
	case errors.Is(err, auth.ErrWeakPassword):
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, i18n.T(r.Context(), "err.profile.password_length"), "")
	case errors.Is(err, auth.ErrPasswordAlreadySet):
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, i18n.T(r.Context(), "err.profile.password_already_set"), "")
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
	}
}

// ChangePassword уничтожает ВСЕ сессии, включая текущую — хендлер тут же выпускает
// новую и переустанавливает cookie, иначе юзер вылетел бы на /login посреди смены пароля.
func (h *Handler) profilePasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// Без rate-limit укравший cookie мог бы перебирать текущий пароль неограниченно;
	// отдельное ключевое пространство ("pw|"+uid) не делит бюджет с /login.
	if !h.loginLimiter.Allow("pw|" + strconv.FormatInt(uid, 10)) {
		h.renderProfile(w, r, http.StatusTooManyRequests, uid, i18n.T(r.Context(), "err.auth.rate_limited"), "")
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	oldPassword := r.FormValue("old")
	newPassword := r.FormValue("new")
	newPassword2 := r.FormValue("new2")

	if newPassword != newPassword2 {
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, i18n.T(r.Context(), "err.profile.new_passwords_differ"), "")
		return
	}

	if err := h.Auth.ChangePassword(r.Context(), uid, oldPassword, newPassword); err != nil {
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, profilePasswordErrorMessage(r.Context(), err), "")
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	h.renderProfile(w, r, http.StatusOK, uid, "", i18n.T(r.Context(), "msg.profile.password_changed"))
}

func profilePasswordErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return i18n.T(ctx, "error.profile_password.invalid_current")
	case errors.Is(err, auth.ErrWeakPassword):
		return i18n.T(ctx, "error.profile_password.weak")
	default:
		return i18n.T(ctx, "error.profile_password.failed")
	}
}

func (h *Handler) profileSessionsRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token, ok := auth.ReadSessionToken(r, h.Secure)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	count, err := h.Auth.DestroyOtherSessions(r.Context(), uid, token)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderProfile(w, r, http.StatusOK, uid, "", revokedSessionsMessage(r.Context(), count))
}

func revokedSessionsMessage(ctx context.Context, count int64) string {
	return i18n.Tf(ctx, "msg.profile.sessions_revoked", "count", strconv.FormatInt(count, 10))
}

// Подтверждение называет email получателя, чтобы опечатка была видна до необратимой передачи.
func (h *Handler) profileInstanceAdminTransfer(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !h.requireInstanceAdminForSSO(w, r, uid) {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if email == "" {
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid,
			i18n.T(r.Context(), "profile.instance_admin.err_email_required"), "")
		return
	}
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.instance_admin_transfer.message",
			"profile.instance_admin.transfer_button", "/profile", "/profile/instance-admin/transfer",
			[]templates.HiddenField{{Name: "email", Value: email}}, "email", email)
		return
	}
	switch _, err := h.Auth.TransferInstanceAdmin(r.Context(), uid, email); {
	case err == nil:
		h.renderProfile(w, r, http.StatusOK, uid, "", i18n.Tf(r.Context(), "profile.instance_admin.transferred", "email", email))
	case errors.Is(err, auth.ErrUserNotFound):
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid,
			i18n.Tf(r.Context(), "profile.instance_admin.err_not_found", "email", email), "")
	case errors.Is(err, auth.ErrSelfTransfer):
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid,
			i18n.T(r.Context(), "profile.instance_admin.err_self"), "")
	case errors.Is(err, auth.ErrNotInstanceAdmin):
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "err.org.sso_admin_only"))
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
	}
}
