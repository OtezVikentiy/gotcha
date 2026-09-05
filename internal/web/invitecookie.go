package web

import (
	"net/http"
	"strings"
)

// inviteNextCookieName — cookie, переносящая токен приглашения через переход
// на /login и /register и обратно (K9-19, it-sec, повтор от 2026-08-27).
//
// Раньше ссылки «войти»/«создать аккаунт» со страницы приглашения
// (InviteAccept) несли next=/invite/{token} в query: адрес с токеном
// оставался в истории браузера, уходил в лог обратного прокси и в Referer
// при переходе с /login или /register вовне. Токен приглашения — это
// учётные данные (получивший его вступает в организацию), и его судьба не
// должна зависеть от того, кто и как читает адресную строку или логи.
//
// Тот же приём, что и у flashCookie (flash.go): значение живёт в HttpOnly
// cookie, а не в адресе. В отличие от flash, эта cookie НЕ гасится сразу при
// чтении — человек может ошибиться паролем или переключиться между входом и
// регистрацией несколько раз, и каждый такой переход обязан по-прежнему
// знать адресата; гасит её только успешный вход/регистрация (auth.go), где
// адресат уже употреблён.
const inviteNextCookieName = "invite_next"

// inviteNextTTL — тот же горизонт, что у oauthFlowTTL (oauthstate.go): это
// intent «уйти войти/зарегистрироваться и вернуться», а не долгоживущее
// состояние.
const inviteNextTTL = 600 // секунд

// setInviteNextCookie запоминает токен приглашения на время ухода на /login
// или /register.
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

// clearInviteNextCookie гасит cookie: вызывается после того, как адресат
// употреблён (успешный вход/регистрация) — независимо от того, был ли он
// в самом деле приглашением, лишний intent не должен пережить следующий вход.
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

// inviteNextToken читает токен приглашения из cookie. Тот же строгий формат,
// что у inviteTokenFromNext (без "/", "?", "#") — cookie полностью
// подконтрольна клиенту, а значение подставляется в путь редиректа
// (inviteAcceptPath), см. комментарий у inviteTokenFromNext про ту же
// причину строгости.
func inviteNextToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(inviteNextCookieName)
	if err != nil || c.Value == "" || strings.ContainsAny(c.Value, "/?#") {
		return "", false
	}
	return c.Value, true
}
