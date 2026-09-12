package web

import (
	"net/http"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

const themeCookie = "theme"

func (h *Handler) withTheme(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		t := h.resolveTheme(w, r)
		next.ServeHTTP(w, r.WithContext(theme.WithTheme(r.Context(), t)))
	})
}

// true, если тема разрешена из cookie — тогда пользовательская ветка пропускается.
func resolveThemeNoUser(r *http.Request) (theme.Theme, bool) {
	if c, err := r.Cookie(themeCookie); err == nil {
		if t, ok := theme.Parse(c.Value); ok {
			return t, true
		}
	}
	return theme.Default, false
}

func (h *Handler) resolveTheme(w http.ResponseWriter, r *http.Request) theme.Theme {
	t, fromCookie := resolveThemeNoUser(r)
	if fromCookie {
		return t
	}
	// без cookie: берём users.theme, иначе засеваем cookie Default — не бить БД на каждый запрос.
	// анонимов не засеваем: иначе будущая пользовательская тема окажется затенена этим cookie.
	if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
		if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
			if code, err := h.Auth.UserTheme(r.Context(), uid); err == nil {
				if ut, ok := theme.Parse(code); ok {
					setThemeCookie(w, ut.Code, h.Secure)
					return ut
				}
				setThemeCookie(w, t.Code, h.Secure)
			}
		}
	}
	return t
}

// не HttpOnly — тема не секрет, клиентский код должен её читать; SameSite=Lax, Secure по схеме.
func setThemeCookie(w http.ResponseWriter, code string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookie,
		Value:    code,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// sameOrigin обязателен — обработчик меняет состояние (POST).
func (h *Handler) themeSwitch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	t, ok := theme.Parse(r.FormValue("theme"))
	if ok {
		setThemeCookie(w, t.Code, h.Secure)
		if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
			if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
				_ = h.Auth.SetTheme(r.Context(), uid, t.Code)
			}
		}
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}
