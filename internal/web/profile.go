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

// profileDelete — POST /profile/delete: самоудаление аккаунта (право субъекта на
// удаление своих ПДн, 152-ФЗ ст.14 / GDPR art.17). Двухшаговое подтверждение,
// как у delete-org (под CSP без inline-JS confirm() невозможен). auth.DeleteUser
// каскадно (FK) удаляет личности/членства/сессии. Блокируется, если юзер —
// единственный владелец каких-то организаций: иначе они остались бы без владельца.
func (h *Handler) profileDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if len(owned) > 0 {
			h.renderError(w, r, http.StatusConflict,
				i18n.Tf(r.Context(), "profile.danger.delete_account.sole_owner", "orgs", strings.Join(owned, ", ")))
			return
		}
	}
	// Без confirmed=yes — страница подтверждения вместо удаления.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.account_delete.message",
			"profile.danger.delete_account.button", "/profile", "/profile/delete", nil)
		return
	}
	if token, ok := auth.ReadSessionToken(r, h.Secure); ok {
		_ = h.Auth.DestroySession(r.Context(), token)
	}
	if err := h.Auth.DeleteUser(r.Context(), uid); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Pending-инвайты на email пользователя не связаны с users по FK, поэтому
	// каскад их не трогает — чистим отдельно (ПДн, минимизация). Best-effort:
	// аккаунт уже удалён, ошибку логируем, но пользователю всё равно редирект.
	// Субъектная телеметрия в ClickHouse (данные КОНЕЧНЫХ пользователей
	// наблюдаемых приложений) при этом не затрагивается — это не ПДн владельца
	// аккаунта; см. privacy-доку.
	if h.Org != nil {
		if email := h.currentEmail(r); email != "" {
			if _, err := h.Org.DeleteInvitesByEmail(r.Context(), email); err != nil {
				slog.Error("profileDelete: purge pending invites", "error", err)
			}
		}
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// profilePage — GET /profile: email юзера, форма смены пароля, кнопка
// «выйти со всех других устройств».
func (h *Handler) profilePage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.renderProfile(w, r, http.StatusOK, uid, "", "")
}

// renderProfile — общий рендер страницы профиля: используется и
// GET-обработчиком, и обоими POST-обработчиками (422 с сообщением об ошибке
// либо 200 с подтверждением — оба на месте, без редиректа, как и
// renderOrgSettings).
func (h *Handler) renderProfile(w http.ResponseWriter, r *http.Request, status int, uid int64, errMsg, message string) {
	email, err := h.Auth.UserEmail(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	hasPassword, err := h.Auth.HasPassword(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	ids, err := h.Auth.ListIdentities(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
			DisplayName: h.providerDisplayName(id.Provider),
			Email:       id.Email,
			CanUnlink:   canUnlink,
		})
	}
	var linkable []templates.LinkableProvider
	if h.OAuth != nil {
		for _, p := range h.OAuth.List() {
			if !linkedNames[p.Name()] {
				linkable = append(linkable, templates.LinkableProvider{Name: p.Name(), DisplayName: p.DisplayName()})
			}
		}
	}
	w.WriteHeader(status)
	_ = templates.Profile(email, errMsg, message, hasPassword, linked, linkable, h.currentEmail(r)).Render(r.Context(), w)
}

// providerDisplayName — человекочитаемое имя провайдера из Registry; fallback —
// сам ключ (провайдер мог быть выключен после привязки).
func (h *Handler) providerDisplayName(name string) string {
	if h.OAuth != nil {
		if p, ok := h.OAuth.Get(name); ok {
			return p.DisplayName()
		}
	}
	return name
}

// profileIdentityUnlink — POST /profile/identities/unlink: отвязывает провайдера
// от аккаунта. Защита последнего способа входа: если у аккаунта нет пароля и
// это его единственная привязка — отказ (409), иначе юзер лишился бы доступа.
func (h *Handler) profileIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	provider := r.FormValue("provider")

	hasPassword, err := h.Auth.HasPassword(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	ids, err := h.Auth.ListIdentities(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// profilePasswordSet — POST /profile/password/set: задаёт пароль аккаунту без
// пароля (OAuth-only). Несовпадение new/new2 или ошибка SetPassword (слабый
// пароль, пароль уже задан) → 422. Успех: пароль задан, сессии не трогаем
// (в отличие от смены пароля) — юзер продолжает работать.
func (h *Handler) profilePasswordSet(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// profilePasswordSubmit — POST /profile/password: old, new, new2. Несовпадение
// new/new2 и любая ошибка auth.ChangePassword (неверный старый пароль, слабый
// новый) — 422. Успех: ChangePassword уже уничтожил ВСЕ сессии пользователя
// (включая текущую), поэтому хендлер тут же выпускает новую сессию и
// переустанавливает cookie — юзер остаётся залогинен, а не выкидывается на
// /login посреди собственной смены пароля.
func (h *Handler) profilePasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// Rate-limit по uid (security fix): без этого укравший cookie может
	// перебирать текущий пароль неограниченно — тот же loginLimiter, что и у
	// /login, но с отдельным ключевым пространством ("pw|"+uid), чтобы не
	// делить бюджет попыток с логином и не зависеть от email/IP.
	if !h.loginLimiter.Allow("pw|" + strconv.FormatInt(uid, 10)) {
		h.renderProfile(w, r, http.StatusTooManyRequests, uid, i18n.T(r.Context(), "err.auth.rate_limited"), "")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	h.renderProfile(w, r, http.StatusOK, uid, "", i18n.T(r.Context(), "msg.profile.password_changed"))
}

// profilePasswordErrorMessage переводит ошибки auth.ChangePassword в
// человекочитаемое сообщение для 422-страницы профиля.
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

// profileSessionsRevoke — POST /profile/sessions/revoke: DestroyOtherSessions
// с токеном ТЕКУЩЕЙ сессии (из cookie запроса) — все остальные сессии
// пользователя (другие устройства/вкладки) уничтожаются, текущая остаётся
// живой. Рендерит страницу профиля с числом завершённых сессий.
func (h *Handler) profileSessionsRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.renderProfile(w, r, http.StatusOK, uid, "", revokedSessionsMessage(r.Context(), count))
}

func revokedSessionsMessage(ctx context.Context, count int64) string {
	return i18n.Tf(ctx, "msg.profile.sessions_revoked", "count", strconv.FormatInt(count, 10))
}
