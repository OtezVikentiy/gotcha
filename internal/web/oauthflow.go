package web

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
)

// secret — ключ подписи oauth-cookie. Пустой SecretKey (стенды) → дефолт.
func (h *Handler) secret() string {
	if h.SecretKey != "" {
		return h.SecretKey
	}
	return "insecure-dev-secret"
}

// oauthRedirectURI — фиксированный callback данного провайдера (не
// конфигурируется, чтобы не разъезжался с тем, что зарегистрировано в IdP).
func (h *Handler) oauthRedirectURI(provider string) string {
	return h.BaseURL + "/auth/oauth/" + provider + "/callback"
}

// sessionUID достаёт uid из сессионной cookie (для роутов без requireUser).
func (h *Handler) sessionUID(r *http.Request) (int64, bool) {
	token, ok := auth.ReadSessionToken(r, h.Secure)
	if !ok {
		return 0, false
	}
	uid, err := h.Auth.SessionUser(r.Context(), token)
	if err != nil {
		return 0, false
	}
	return uid, true
}

// oauthStart — GET /auth/oauth/{provider}/start: генерит state/nonce/PKCE,
// кладёт их в подписанную короткоживущую cookie и редиректит на страницу
// согласия провайдера. ?link=1 (для потока привязки из профиля) требует
// активной сессии; иначе поток обычного входа.
func (h *Handler) oauthStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, _, ok := h.resolveProvider(r.Context(), name)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.oauth.unknown_provider"))
		return
	}
	link := r.URL.Query().Get("link") == "1"
	var uid int64
	if link {
		id, ok := h.sessionUID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		uid = id
	}
	state, err := oauth.RandomToken()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	nonce, err := oauth.RandomToken()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	verifier, challenge, err := oauth.PKCE()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	flow := oauthFlow{
		Provider: name, State: state, Nonce: nonce, Verifier: verifier,
		Link: link, UID: uid, IssuedAt: time.Now().Unix(),
	}
	raw, err := signFlow([]byte(h.secret()), flow)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookieName,
		Value:    raw,
		Path:     "/auth/oauth",
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthFlowTTL,
	})
	authURL := p.AuthURL(state, nonce, challenge, h.oauthRedirectURI(name))
	if authURL == "" {
		slog.Error("oauth authURL empty", "provider", name)
		h.renderError(w, r, http.StatusBadGateway, i18n.T(r.Context(), "error.oauth.provider_unavailable"))
		return
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// oauthCallback — GET /auth/oauth/{provider}/callback: проверяет state,
// меняет код на Identity и решает провижининг (link-only/invite-gated).
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, sso, ok := h.resolveProvider(r.Context(), name)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.oauth.unknown_provider"))
		return
	}
	// Cookie одноразовая: стираем сразу, независимо от исхода.
	c, err := r.Cookie(oauthCookieName)
	clearOAuthCookie(w, h.Secure)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.oauth.session_expired"))
		return
	}
	flow, err := parseFlow([]byte(h.secret()), c.Value, time.Now().Unix())
	if err != nil || flow.Provider != name {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.oauth.session_expired"))
		return
	}
	if flow.State == "" || r.URL.Query().Get("state") != flow.State {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.oauth.invalid_state"))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.oauthFail(w, r, name)
		return
	}
	id, err := p.Exchange(r.Context(), code, flow.Verifier, h.oauthRedirectURI(name), flow.Nonce)
	if err != nil || id.Subject == "" || id.Email == "" {
		slog.Warn("oauth exchange failed", "provider", name, "err", err)
		h.oauthFail(w, r, name)
		return
	}

	// Per-org SSO (этап 10): своя ветка — domain guard + JIT-провижининг.
	if sso != nil {
		h.ssoCallback(w, r, name, id, sso)
		return
	}

	// SEC-H2 / RA-L2: если домен email принадлежит организации с enforced-SSO,
	// env-провайдер (личный/инстансовый Яндекс/VK/OIDC) не может выдать сессию —
	// только собственный IdP организации. /sso — identifier-first, направит на нужный
	// SSO-start. Guard стоит до link-ветки: привязка identity к enforced-домену через
	// env-провайдера тоже блокируется, что корректно (централизованный provisioning).
	// Гейт НЕ зависит от id.EmailVerified: generic-OIDC без email_verified иначе
	// проскакивал бы мимо гейта в ветку «login by subject» (RA-L2). Домен нормализован
	// в emailDomain (регистр + trailing-dot), чтобы "user@enforced.com." не обходил гейт.
	if h.enforcedSSO(r.Context(), emailDomain(id.Email)) {
		http.Redirect(w, r, "/sso", http.StatusSeeOther)
		return
	}

	// 1) Вход по стабильному субъекту.
	if uid, err := h.Auth.IdentityUser(r.Context(), name, id.Subject); err == nil {
		_ = h.Auth.UpdateIdentityEmail(r.Context(), name, id.Subject, id.Email)
		h.oauthLogin(w, r, uid, "/")
		return
	} else if !errors.Is(err, auth.ErrNoIdentity) {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// 2) Поток привязки из профиля. Доверяем ТОЛЬКО текущей сессии, а не UID из
	// cookie (SEC-C1: при утёкшем ключе подписи UID в cookie подделывается — линковка
	// к чужому аккаунту). Реальную привязку разрешаем лишь к залогиненному юзеру.
	if flow.Link {
		uid, ok := h.sessionUID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		switch err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); {
		case err == nil:
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		case errors.Is(err, auth.ErrIdentityTaken):
			h.renderError(w, r, http.StatusConflict, i18n.Tf(r.Context(), "error.oauth.already_linked", "provider", p.DisplayName()))
		case errors.Is(err, auth.ErrAlreadyLinked):
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		default:
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		}
		return
	}

	// 3) Неявная привязка по verified email к существующему аккаунту.
	uid, err := h.Auth.UserByEmail(r.Context(), id.Email)
	switch {
	case err == nil:
		// Неявная привязка к УЖЕ существующему аккаунту допустима только когда
		// провайдер сам доверенный источник верификации email (VK/Яндекс). Для
		// generic-OIDC email_verified контролирует произвольный IdP — доверять
		// ему для auto-link нельзя (иначе IdP, заявивший чужой адрес, угнал бы
		// парольный аккаунт). Тогда — вход паролем и ручная привязка в /profile.
		if !id.EmailVerified || !id.TrustedIssuer {
			h.renderError(w, r, http.StatusForbidden,
				i18n.T(r.Context(), "error.oauth.email_not_verified_link_profile"))
			return
		}
		if err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); err != nil &&
			!errors.Is(err, auth.ErrAlreadyLinked) {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.oauthLogin(w, r, uid, "/")
	case errors.Is(err, auth.ErrUserNotFound):
		h.oauthProvisionByInvite(w, r, name, id)
	default:
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// oauthProvisionByInvite заводит аккаунт по действующему инвайту на verified
// email и логинит. Нет инвайта → отказ, аккаунт не создаётся.
func (h *Handler) oauthProvisionByInvite(w http.ResponseWriter, r *http.Request, provider string, id oauth.Identity) {
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.provider_no_email"))
		return
	}
	has, err := h.Org.HasPendingInvite(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !has {
		h.renderError(w, r, http.StatusForbidden,
			i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	uid, err := h.Auth.CreateOAuthUser(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if _, ok, err := h.Org.AcceptPendingInviteByEmail(r.Context(), id.Email, uid); err != nil || !ok {
		// Гонка: инвайт исчез между проверкой и принятием — откатываем юзера.
		_ = h.Auth.DeleteUser(r.Context(), uid)
		h.renderError(w, r, http.StatusForbidden,
			i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	if err := h.Auth.LinkIdentity(r.Context(), uid, provider, id.Subject, id.Email); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.oauthLogin(w, r, uid, "/")
}

// oauthLogin выпускает сессию и редиректит.
func (h *Handler) oauthLogin(w http.ResponseWriter, r *http.Request, uid int64, dest string) {
	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// oauthFail — нейтральная страница ошибки провайдера (без утечки деталей).
func (h *Handler) oauthFail(w http.ResponseWriter, r *http.Request, provider string) {
	name := provider
	if p, ok := h.OAuth.Get(provider); ok {
		name = p.DisplayName()
	}
	h.renderError(w, r, http.StatusBadGateway, i18n.Tf(r.Context(), "error.oauth.login_failed", "provider", name))
}

func clearOAuthCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookieName, Value: "", Path: "/auth/oauth",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
