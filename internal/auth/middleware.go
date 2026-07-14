package auth

import (
	"context"
	"errors"
	"net/http"
)

// CookieName — имя сессионной cookie.
const CookieName = "gotcha_session"

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
		c, err := r.Cookie(CookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		uid, err := s.SessionUser(r.Context(), c.Value)
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
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearSessionCookie стирает сессионную cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
