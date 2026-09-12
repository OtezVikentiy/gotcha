package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const authFormMaxBodyBytes = 8 << 10

func providerLabel(ctx context.Context, name, displayName string) string {
	key := "oauth.provider." + name
	if s := i18n.T(ctx, key); s != key {
		return s
	}
	return displayName
}

func (h *Handler) oauthButtons(ctx context.Context) []templates.OAuthButton {
	if h.OAuth == nil {
		return nil
	}
	var out []templates.OAuthButton
	for _, p := range h.OAuth.List() {
		out = append(out, templates.OAuthButton{Name: p.Name(), Label: i18n.Tf(ctx, "auth.oauth.login_with", "provider", providerLabel(ctx, p.Name(), p.DisplayName()))})
	}
	return out
}

func (h *Handler) resolveAuthNext(w http.ResponseWriter, r *http.Request, rawNext string) string {
	next := safeNextPath(rawNext)
	if next != "" {
		if token, ok := inviteTokenFromNext(next); ok {
			h.setInviteNextCookie(w, token)
		}
		return next
	}
	if token, ok := inviteNextToken(r); ok {
		return inviteAcceptPath(token)
	}
	return ""
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	next := h.resolveAuthNext(w, r, r.URL.Query().Get("next"))
	_ = templates.Login("", next, "", h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	next := h.resolveAuthNext(w, r, r.URL.Query().Get("next"))
	if h.registrationClosed(r) {
		_ = templates.RegisterStub("", h.RegistrationMode, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}
	_ = templates.RegisterForm("", h.inviteOnlyNotice(r, next), next, h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) inviteOnlyNotice(r *http.Request, next string) bool {
	if h.RegistrationMode != "invite" {
		return false
	}
	if _, ok := inviteTokenFromNext(next); ok {
		return false
	}
	n, err := h.Auth.UserCount(r.Context())
	if err != nil {
		return true
	}
	return n > 0
}

func (h *Handler) registrationClosed(r *http.Request) bool {
	if h.RegistrationMode != "closed" {
		return false
	}
	n, err := h.Auth.UserCount(r.Context())
	if err != nil {
		return false
	}
	return n > 0
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Разбор строгий: ровно /invite/{token}, без лишних сегментов и без query —
// вольность здесь стала бы новой поверхностью атаки.
func inviteTokenFromNext(next string) (string, bool) {
	const prefix = "/invite/"
	if !strings.HasPrefix(next, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(next, prefix)
	if token == "" || strings.ContainsAny(token, "/?#") {
		return "", false
	}
	return token, true
}

// Нужны И живой токен, И совпадение email с приглашением — одного токена
// мало: утёкшая ссылка иначе заводила бы аккаунт под произвольным адресом.
func (h *Handler) invitedByToken(w http.ResponseWriter, r *http.Request, next, email string) bool {
	if h.RegistrationMode != "invite" || h.Org == nil {
		h.denyRegistration(w, r, next, "mode_closed")
		return false
	}
	token, ok := inviteTokenFromNext(next)
	if !ok {
		h.denyRegistration(w, r, next, "no_token")
		return false
	}
	inv, err := h.Org.InviteByToken(r.Context(), token)
	if errors.Is(err, org.ErrInviteInvalid) {
		h.denyRegistration(w, r, next, "bad_token")
		return false
	}
	if err != nil {
		// Fail closed: не знаем, действительно ли приглашение — не заводим аккаунт.
		slog.Error("register: invite lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return false
	}
	if !strings.EqualFold(inv.Email, email) {
		h.denyRegistration(w, r, next, "email_mismatch")
		return false
	}
	return true
}

// Причины отказа внутри invite обязаны выглядеть одинаково клиенту — иначе
// reason становится оракулом enumeration (в лог пишется он, не сам адрес).
func (h *Handler) denyRegistration(w http.ResponseWriter, r *http.Request, next, reason string) {
	slog.Warn("register: denied", "reason", reason, "ip", h.clientIP(r))
	if token, ok := inviteTokenFromNext(next); ok {
		h.setInviteNextCookie(w, token)
	}
	msg := i18n.T(r.Context(), "auth.register.invite_required")
	if h.RegistrationMode == "closed" {
		msg = i18n.T(r.Context(), "auth.register.closed_denied")
	}
	w.WriteHeader(http.StatusForbidden)
	_ = templates.RegisterStub(msg, h.RegistrationMode, next,
		h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	// ipLimiter — первый операнд ||: иначе поток запросов с произвольным
	// email переполняет карту loginLimiter раньше, чем сработает сам ipLimiter.
	emailKey := limiterEmailKeyPart(email)
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow(h.rateLimitKey(r, email)) ||
		!h.emailLimiter.Allow(emailKey) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.rate_limited_login"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	enforced, err := h.enforcedSSO(r.Context(), emailDomain(email))
	if err != nil {
		// Fail closed: неизвестно, обязателен ли SSO для домена — не пускаем.
		slog.Error("login: enforced SSO lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if enforced {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.sso_required"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.bad_credentials"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	h.clearInviteNextCookie(w)
	redirectLocal(w, r, safeNextPath(r.FormValue("next")))
}

func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	next := safeNextPath(r.FormValue("next"))

	// Порядок как в loginSubmit: ipLimiter первым в || — иначе он не успевает
	// сработать раньше переполнения карты loginLimiter потоком email.
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow(h.rateLimitKey(r, email)) ||
		!h.emailLimiter.Allow(limiterEmailKeyPart(email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.rate_limited_register"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	if h.RegistrationMode != "open" {
		n, err := h.Auth.UserCount(r.Context())
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if n > 0 && !h.invitedByToken(w, r, next, normalizeEmail(email)) {
			return
		}
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.passwords_differ"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	enforced, err := h.enforcedSSO(r.Context(), emailDomain(email))
	if err != nil {
		// Fail closed: домен может требовать SSO — регистрацию паролем не даём.
		slog.Error("register: enforced SSO lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if enforced {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.sso_required"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.RegisterForm(registerErrorMessage(r.Context(), err), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// Членство сюда не выдаётся: его выдаёт только AcceptInvite по токену,
	// после входа — совпадения email с приглашением для этого недостаточно.
	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	h.clearInviteNextCookie(w)
	redirectLocal(w, r, next)
}

func registerErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		// Не раскрываем существование аккаунта: нейтральный текст, не
		// «этот email уже зарегистрирован».
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
		h.denyCrossOrigin(w, r)
		return
	}
	if token, ok := auth.ReadSessionToken(r, h.Secure); ok {
		_ = h.Auth.DestroySession(r.Context(), token)
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) ssoPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.SSOLogin("").Render(r.Context(), w)
}

func (h *Handler) ssoSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	// Тот же loginLimiter, что у /login и /register — ipLimiter первым в ||,
	// иначе /sso обходит его защиту от переполнения карты.
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow("sso|"+h.rateLimitKey(r, email)) {
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
