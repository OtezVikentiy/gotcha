package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// oauthButtons собирает кнопки включённых провайдеров для страниц входа.
func (h *Handler) oauthButtons(ctx context.Context) []templates.OAuthButton {
	if h.OAuth == nil {
		return nil
	}
	var out []templates.OAuthButton
	for _, p := range h.OAuth.List() {
		out = append(out, templates.OAuthButton{Name: p.Name(), Label: i18n.Tf(ctx, "auth.oauth.login_with", "provider", p.DisplayName())})
	}
	return out
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Login("", h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	// PROD-B1: если режим не open и первый пользователь уже есть — показываем
	// экран «регистрация по приглашению» вместо формы (bootstrap уже пройден).
	if h.registrationClosed(r) {
		_ = templates.Register(i18n.T(r.Context(), "error.register.closed"), true, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}
	_ = templates.Register("", false, h.oauthButtons(r.Context())).Render(r.Context(), w)
}

// registrationClosed сообщает, закрыта ли сейчас самостоятельная парольная
// регистрация: режим не open и на инстансе уже есть пользователь (bootstrap
// первого админа пройден). Ошибку подсчёта трактуем как «не закрыто», чтобы не
// прятать форму из-за временного сбоя БД — фактический гейтинг в registerSubmit.
func (h *Handler) registrationClosed(r *http.Request) bool {
	if h.RegistrationMode == "open" {
		return false
	}
	n, err := h.Auth.UserCount(r.Context())
	if err != nil {
		return false
	}
	return n > 0
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

	// SEC-L2: сначала per-account (ip|email), затем глобальный per-IP лимит.
	// Любое превышение → 429. Порядок важен: при исчерпанном per-account слот
	// per-IP не расходуется (короткое замыкание ||).
	if !h.loginLimiter.Allow(rateLimitKey(r, email)) || !h.ipLimiter.Allow(extractIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.rate_limited_login"), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// Принуждение SSO (этап 10, SEC-H2): если домен email принадлежит организации с
	// enforced-SSO, пароль не принимаем — только вход через SSO.
	if h.enforcedSSO(r.Context(), emailDomain(email)) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.sso_required"), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.bad_credentials"), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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

	// SEC-L2: per-account (ip|email) + глобальный per-IP лимит, см. loginSubmit.
	if !h.loginLimiter.Allow(rateLimitKey(r, email)) || !h.ipLimiter.Allow(extractIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.rate_limited_register"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// PROD-B1: гейтинг регистрации по режиму. Первый пользователь инстанса
	// всегда может зарегистрироваться (bootstrap инстанс-админа); дальше — по
	// режиму. open — всегда открыто; invite/closed — только bootstrap первого.
	if h.RegistrationMode != "open" {
		n, err := h.Auth.UserCount(r.Context())
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if n > 0 {
			w.WriteHeader(http.StatusForbidden)
			_ = templates.Register(i18n.T(r.Context(), "error.register.closed"), true, h.oauthButtons(r.Context())).Render(r.Context(), w)
			return
		}
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.passwords_differ"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// SEC-H2: домен с enforced-SSO не может регистрироваться паролем (обход
	// централизованного provisioning/деprovisioning). Как в loginSubmit.
	if h.enforcedSSO(r.Context(), emailDomain(email)) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.sso_required"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(registerErrorMessage(r.Context(), err), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func registerErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		// SEC-L1: не раскрываем существование аккаунта (enumeration) —
		// нейтральная формулировка вместо «этот email уже зарегистрирован».
		return i18n.T(ctx, "error.register.email_taken")
	case errors.Is(err, auth.ErrWeakPassword):
		return i18n.T(ctx, "error.register.weak_password")
	case errors.Is(err, auth.ErrInvalidEmail):
		return i18n.T(ctx, "error.register.invalid_email")
	default:
		return i18n.T(ctx, "error.register.failed")
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if token, ok := auth.ReadSessionToken(r, h.Secure); ok {
		_ = h.Auth.DestroySession(r.Context(), token)
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
		_ = templates.SSOLogin(i18n.T(r.Context(), "err.auth.rate_limited")).Render(r.Context(), w)
		return
	}
	cfg, ok, err := h.Org.SSOByDomain(r.Context(), emailDomain(email))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.SSOLogin(i18n.T(r.Context(), "err.auth.sso_not_configured")).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/auth/oauth/"+ssoProviderPrefix+strconv.FormatInt(cfg.OrgID, 10)+"/start", http.StatusSeeOther)
}
