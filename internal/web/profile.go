package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

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
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(status)
	_ = templates.Profile(email, errMsg, message, h.currentEmail(r)).Render(r.Context(), w)
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
		h.renderProfile(w, r, http.StatusTooManyRequests, uid, "слишком много попыток, попробуйте через минуту", "")
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
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, "новые пароли не совпадают", "")
		return
	}

	if err := h.Auth.ChangePassword(r.Context(), uid, oldPassword, newPassword); err != nil {
		h.renderProfile(w, r, http.StatusUnprocessableEntity, uid, profilePasswordErrorMessage(err), "")
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	h.renderProfile(w, r, http.StatusOK, uid, "", "пароль изменён")
}

// profilePasswordErrorMessage переводит ошибки auth.ChangePassword в
// человекочитаемое сообщение для 422-страницы профиля.
func profilePasswordErrorMessage(err error) string {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return "неверный текущий пароль"
	case errors.Is(err, auth.ErrWeakPassword):
		return "новый пароль должен быть от 8 до 512 символов"
	default:
		return "не удалось изменить пароль"
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
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	count, err := h.Auth.DestroyOtherSessions(r.Context(), uid, c.Value)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.renderProfile(w, r, http.StatusOK, uid, "", revokedSessionsMessage(count))
}

func revokedSessionsMessage(count int64) string {
	return fmt.Sprintf("Завершено сеансов на других устройствах: %d", count)
}
