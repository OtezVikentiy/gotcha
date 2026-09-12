package web

import (
	"net/http"
	"strings"
)

// Токен приглашения — учётные данные: в query он утёк бы в историю браузера, лог прокси и
// Referer. Не гасится при чтении (в отличие от flashCookie) — только при успешном входе.
const inviteNextCookieName = "invite_next"

// Короткий intent «уйти войти и вернуться», не долгоживущее состояние (как oauthFlowTTL).
const inviteNextTTL = 600 // секунд

func (h *Handler) setInviteNextCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     inviteNextCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   inviteNextTTL,
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Вызывается после успешного входа/регистрации независимо от того, было ли приглашение —
// лишний intent не должен пережить следующий вход.
func (h *Handler) clearInviteNextCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     inviteNextCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Строгий формат (без "/", "?", "#"): cookie полностью подконтрольна клиенту, а значение
// подставляется в путь редиректа (inviteAcceptPath).
func inviteNextToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(inviteNextCookieName)
	if err != nil || c.Value == "" || strings.ContainsAny(c.Value, "/?#") {
		return "", false
	}
	return c.Value, true
}
