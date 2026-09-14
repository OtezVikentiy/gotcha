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

func (h *Handler) secret() string {
	if h.SecretKey != "" {
		return h.SecretKey
	}
	return "insecure-dev-secret"
}

func (h *Handler) oauthRedirectURI(provider string) string {
	return h.BaseURL + "/auth/oauth/" + provider + "/callback"
}

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
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	nonce, err := oauth.RandomToken()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	verifier, challenge, err := oauth.PKCE()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	flow := oauthFlow{
		Provider: name, State: state, Nonce: nonce, Verifier: verifier,
		Link: link, UID: uid, IssuedAt: time.Now().Unix(),
	}
	raw, err := signFlow([]byte(h.secret()), flow)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
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
		h.oauthFail(w, r, name, p)
		return
	}
	id, err := p.Exchange(r.Context(), code, flow.Verifier, h.oauthRedirectURI(name), flow.Nonce)
	if err != nil || id.Subject == "" || id.Email == "" {
		slog.Warn("oauth exchange failed", "provider", name, "err", err)
		h.oauthFail(w, r, name, p)
		return
	}

	if sso != nil {
		h.ssoCallback(w, r, name, id, sso, flow)
		return
	}

	// Гейт до входа/линковки: иначе env-провайдер обошёл бы enforced-SSO при привязке или входе.
	// Не зависит от EmailVerified; домен нормализован (регистр, trailing dot) против обхода точкой.
	enforced, err := h.enforcedSSO(r.Context(), emailDomain(id.Email))
	if err != nil {
		// Fail closed: гейт недоступен — уводим на /sso, не пропуская мимо централизованного provisioning.
		slog.Error("oauth: enforced SSO lookup failed", "error", err)
		http.Redirect(w, r, "/sso", http.StatusSeeOther)
		return
	}
	if enforced {
		http.Redirect(w, r, "/sso", http.StatusSeeOther)
		return
	}

	if uid, err := h.Auth.IdentityUser(r.Context(), name, id.Subject); err == nil {
		_ = h.Auth.UpdateIdentityEmail(r.Context(), name, id.Subject, id.Email)
		h.oauthLogin(w, r, uid, "/")
		return
	} else if !errors.Is(err, auth.ErrNoIdentity) {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	// UID из cookie должен совпасть с UID сессии: одна лишь сессия допускает захват чужим link-потоком,
	// одна лишь cookie — подделку при утёкшем ключе подписи.
	if flow.Link {
		uid, ok := h.sessionUID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if flow.UID != uid {
			h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.oauth.session_expired"))
			return
		}
		switch err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); {
		case err == nil:
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		case errors.Is(err, auth.ErrIdentityTaken):
			h.renderError(w, r, http.StatusConflict, i18n.Tf(r.Context(), "error.oauth.already_linked", "provider", providerLabel(r.Context(), name, p.DisplayName())))
		case errors.Is(err, auth.ErrAlreadyLinked):
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		default:
			h.renderError(w, r, http.StatusInternalServerError, "")
		}
		return
	}

	uid, err := h.Auth.UserByEmail(r.Context(), id.Email)
	switch {
	case err == nil:
		// Автопривязка по email разрешена только доверенным провайдерам (VK/Яндекс): для generic-OIDC
		// email_verified подделывается произвольным IdP — иначе угон парольного аккаунта чужим адресом.
		if !id.EmailVerified || !id.TrustedIssuer {
			h.renderError(w, r, http.StatusForbidden,
				i18n.T(r.Context(), "error.oauth.email_not_verified_link_profile"))
			return
		}
		if err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); err != nil &&
			!errors.Is(err, auth.ErrAlreadyLinked) {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		h.oauthLogin(w, r, uid, "/")
	case errors.Is(err, auth.ErrUserNotFound):
		h.oauthProvision(w, r, name, id)
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
	}
}

func (h *Handler) oauthProvision(w http.ResponseWriter, r *http.Request, provider string, id oauth.Identity) {
	if h.RegistrationMode == "closed" {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.provider_no_email"))
		return
	}
	// Провижининг доверяет email издателя не меньше линковки к существующему аккаунту:
	// иначе self-service OIDC-тенант подделывает email и входит по чужому приглашению.
	if !id.TrustedIssuer {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.provider_not_trusted"))
		return
	}
	if h.RegistrationMode == "open" {
		uid, err := h.Auth.CreateOAuthUser(r.Context(), id.Email)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		// Привязка identity обязана идти до принятия инвайта: сбой откатывает юзера,
		// а accepted_at откат уже не вернёт — иначе приглашение сгорает.
		if err := h.Auth.LinkIdentity(r.Context(), uid, provider, id.Subject, id.Email); err != nil {
			_ = h.Auth.DeleteUser(r.Context(), uid)
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if _, _, err := h.Org.AcceptPendingInviteByEmail(r.Context(), id.Email, uid); err != nil {
			slog.Warn("oauth open provisioning: accept pending invite", "err", err)
		}
		h.oauthLogin(w, r, uid, "/")
		return
	}
	has, err := h.Org.HasPendingInvite(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !has {
		h.renderError(w, r, http.StatusForbidden,
			i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	uid, err := h.Auth.CreateOAuthUser(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	// Привязка identity обязана идти до принятия инвайта: сбой откатывает юзера,
	// а accepted_at откат уже не вернёт — иначе приглашение сгорает.
	if err := h.Auth.LinkIdentity(r.Context(), uid, provider, id.Subject, id.Email); err != nil {
		_ = h.Auth.DeleteUser(r.Context(), uid)
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, ok, err := h.Org.AcceptPendingInviteByEmail(r.Context(), id.Email, uid); err != nil || !ok {
		// Гонка между проверкой и принятием инвайта — откатываем юзера, DeleteUser каскадит identity.
		_ = h.Auth.DeleteUser(r.Context(), uid)
		h.renderError(w, r, http.StatusForbidden,
			i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	h.oauthLogin(w, r, uid, "/")
}

func (h *Handler) oauthLogin(w http.ResponseWriter, r *http.Request, uid int64, dest string) {
	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (h *Handler) oauthFail(w http.ResponseWriter, r *http.Request, provider string, p oauth.Provider) {
	name := provider
	if p != nil {
		name = providerLabel(r.Context(), provider, p.DisplayName())
	}
	h.renderError(w, r, http.StatusBadGateway, i18n.Tf(r.Context(), "error.oauth.login_failed", "provider", name))
}

func clearOAuthCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookieName, Value: "", Path: "/auth/oauth",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
