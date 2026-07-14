package web

import (
	"errors"
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Login("").Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Register("").Render(r.Context(), w)
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	if !h.loginLimiter.Allow(rateLimitKey(r, email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Login("слишком много попыток входа, попробуйте через минуту").Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login("неверный email или пароль").Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")

	if !h.loginLimiter.Allow(rateLimitKey(r, email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Register("слишком много попыток регистрации, попробуйте через минуту").Render(r.Context(), w)
		return
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register("пароли не совпадают").Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(registerErrorMessage(err)).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func registerErrorMessage(err error) string {
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		return "этот email уже зарегистрирован"
	case errors.Is(err, auth.ErrWeakPassword):
		return "пароль должен быть от 8 до 512 символов"
	case errors.Is(err, auth.ErrInvalidEmail):
		return "неверный формат email"
	default:
		return "не удалось зарегистрироваться"
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		_ = h.Auth.DestroySession(r.Context(), c.Value)
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
