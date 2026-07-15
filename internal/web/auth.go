package web

import (
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// oauthButtons собирает кнопки включённых провайдеров для страниц входа.
func (h *Handler) oauthButtons() []templates.OAuthButton {
	if h.OAuth == nil {
		return nil
	}
	var out []templates.OAuthButton
	for _, p := range h.OAuth.List() {
		out = append(out, templates.OAuthButton{Name: p.Name(), Label: "Войти через " + p.DisplayName()})
	}
	return out
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Login("", h.oauthButtons()).Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Register("", h.oauthButtons()).Render(r.Context(), w)
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
		_ = templates.Login("слишком много попыток входа, попробуйте через минуту", h.oauthButtons()).Render(r.Context(), w)
		return
	}

	// Принуждение SSO (этап 10): если домен email принадлежит организации с
	// enforced-SSO, пароль не принимаем — только вход через SSO.
	if cfg, ok, err := h.Org.SSOByDomain(r.Context(), emailDomain(email)); err == nil && ok && cfg.Enforced {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login("ваша организация требует вход через SSO — используйте «Вход через SSO»", h.oauthButtons()).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login("неверный email или пароль", h.oauthButtons()).Render(r.Context(), w)
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
		_ = templates.Register("слишком много попыток регистрации, попробуйте через минуту", h.oauthButtons()).Render(r.Context(), w)
		return
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register("пароли не совпадают", h.oauthButtons()).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(registerErrorMessage(err), h.oauthButtons()).Render(r.Context(), w)
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

// ssoPage — GET /sso: identifier-first вход (этап 10). Поле email → по домену
// резолвим SSO организации.
func (h *Handler) ssoPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.SSOLogin("").Render(r.Context(), w)
}

// ssoSubmit — POST /sso: резолв org_sso по email-домену → редирект на SSO-start
// организации. Неизвестный домен → нейтральное сообщение (не палим список доменов).
func (h *Handler) ssoSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	if !h.loginLimiter.Allow("sso|" + rateLimitKey(r, email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.SSOLogin("слишком много попыток, попробуйте через минуту").Render(r.Context(), w)
		return
	}
	cfg, ok, err := h.Org.SSOByDomain(r.Context(), emailDomain(email))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.SSOLogin("SSO для этого домена не настроен").Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/auth/oauth/"+ssoProviderPrefix+strconv.FormatInt(cfg.OrgID, 10)+"/start", http.StatusSeeOther)
}
