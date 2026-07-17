package auth

import (
	"context"
	"errors"
	"net/http"
)

// CookieName — имя сессионной cookie на plain-http.
const CookieName = "gotcha_session"

// hostCookieName — имя сессионной cookie на HTTPS. Префикс __Host- заставляет
// браузер отвергнуть cookie без Secure, без Path=/ или с Domain — это защищает
// от подмены cookie поддоменом/по plain-http.
const hostCookieName = "__Host-gotcha_session"

// sessionCookieName выбирает имя cookie по схеме: на HTTPS — префиксное __Host-,
// на plain-http — обычное (там __Host- невозможен, т.к. требует Secure).
func sessionCookieName(secure bool) string {
	if secure {
		return hostCookieName
	}
	return CookieName
}

// ReadSessionToken достаёт сессионный токен. Всегда сначала пробует префиксное
// имя (__Host-). Непрефиксное gotcha_session принимается ТОЛЬКО на plain-http
// (secure=false) — ради совместимости и смены схемы http↔https без разлогина.
//
// RA-L1: на HTTPS (secure=true) непрефиксную cookie игнорируем. Иначе смысл
// __Host- теряется: поддомен или MITM на plain-http мог бы навязать
// непрефиксную gotcha_session и провести узкий pre-login session-fixation.
func ReadSessionToken(r *http.Request, secure bool) (string, bool) {
	if c, err := r.Cookie(hostCookieName); err == nil {
		return c.Value, true
	}
	if secure {
		return "", false
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value, true
	}
	return "", false
}

type ctxKey struct{}

// UserID достаёт id аутентифицированного пользователя из контекста запроса.
func UserID(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok
}

// RequireUser пропускает только запросы с живой сессией; остальных
// отправляет на /login. UserID кладётся в context запроса.
func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := ReadSessionToken(r, s.Secure)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		uid, err := s.SessionUser(r.Context(), token)
		switch {
		case errors.Is(err, ErrNoSession):
			ClearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		case err != nil:
			// Инфраструктурная ошибка — не трогаем cookie юзера.
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, uid)))
	})
}

// SetSessionCookie выставляет сессионную cookie. secure приходит от
// вызывающего (BaseURL начинается с https:// → true): безусловный Secure
// сломал бы логин на plain-http self-hosted инсталляциях.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(secure),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearSessionCookie стирает сессионную cookie под обоими именами, чтобы logout
// сработал независимо от схемы, под которой cookie была выставлена.
func ClearSessionCookie(w http.ResponseWriter) {
	for _, name := range []string{CookieName, hostCookieName} {
		// Для __Host- нужен Secure, иначе браузер по HTTPS отвергнет и удаление.
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   name == hostCookieName,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}
