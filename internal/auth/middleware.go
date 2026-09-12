package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

const CookieName = "gotcha_session"

// Префикс __Host- заставляет браузер отвергнуть cookie без Secure/Path=/ или с Domain —
// защита от подмены cookie поддоменом или по plain-http.
const hostCookieName = "__Host-gotcha_session"

// __Host- требует Secure и на plain-http невозможен — там имя обычное, на HTTPS префиксное.
func sessionCookieName(secure bool) string {
	if secure {
		return hostCookieName
	}
	return CookieName
}

// На HTTPS непрефиксную cookie игнорируем — иначе поддомен или MITM на plain-http навяжут её,
// проведя pre-login session-fixation в обход __Host-.
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

func UserID(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok
}

// Без next приглашения и ссылки из писем вели бы после входа на главную; next сохраняем только
// для безопасных методов — POST молча не повторяем, перенаправляя после входа.
func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := ReadSessionToken(r, s.Secure)
		if !ok {
			http.Redirect(w, r, loginWithNext(r), http.StatusSeeOther)
			return
		}
		uid, err := s.SessionUser(r.Context(), token)
		switch {
		case errors.Is(err, ErrNoSession):
			ClearSessionCookie(w)
			http.Redirect(w, r, loginWithNext(r), http.StatusSeeOther)
			return
		case err != nil:
			// Инфраструктурная ошибка — не трогаем cookie юзера.
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, uid)))
	})
}

// Secure всегда true сломал бы логин на plain-http self-hosted — значение приходит от BaseURL
// (https:// → true).
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

// Чистит оба имени cookie — logout должен сработать независимо от схемы, под которой её выставили.
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

// Путь берётся из запроса — локальный, но через форму может быть подменён;
// валидирует его safeNextPath в пакете web.
func loginWithNext(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "/login"
	}
	next := r.URL.RequestURI()
	if next == "" || next == "/" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}
