package web

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
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
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return 0, false
	}
	uid, err := h.Auth.SessionUser(r.Context(), c.Value)
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
		h.renderError(w, r, http.StatusNotFound, "unknown provider")
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
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	nonce, err := oauth.RandomToken()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	verifier, challenge, err := oauth.PKCE()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	flow := oauthFlow{
		Provider: name, State: state, Nonce: nonce, Verifier: verifier,
		Link: link, UID: uid, IssuedAt: time.Now().Unix(),
	}
	raw, err := signFlow([]byte(h.secret()), flow)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
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
		h.renderError(w, r, http.StatusBadGateway, "провайдер недоступен")
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
		h.renderError(w, r, http.StatusNotFound, "unknown provider")
		return
	}
	// Cookie одноразовая: стираем сразу, независимо от исхода.
	c, err := r.Cookie(oauthCookieName)
	clearOAuthCookie(w, h.Secure)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "сессия входа истекла, попробуйте снова")
		return
	}
	flow, err := parseFlow([]byte(h.secret()), c.Value, time.Now().Unix())
	if err != nil || flow.Provider != name {
		h.renderError(w, r, http.StatusBadRequest, "сессия входа истекла, попробуйте снова")
		return
	}
	if flow.State == "" || r.URL.Query().Get("state") != flow.State {
		h.renderError(w, r, http.StatusBadRequest, "неверный state (возможная CSRF-атака)")
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

	// 1) Вход по стабильному субъекту.
	if uid, err := h.Auth.IdentityUser(r.Context(), name, id.Subject); err == nil {
		_ = h.Auth.UpdateIdentityEmail(r.Context(), name, id.Subject, id.Email)
		h.oauthLogin(w, r, uid, "/")
		return
	} else if !errors.Is(err, auth.ErrNoIdentity) {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// 2) Поток привязки из профиля.
	if flow.Link && flow.UID != 0 {
		switch err := h.Auth.LinkIdentity(r.Context(), flow.UID, name, id.Subject, id.Email); {
		case err == nil:
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		case errors.Is(err, auth.ErrIdentityTaken):
			h.renderError(w, r, http.StatusConflict, "этот аккаунт "+p.DisplayName()+" уже привязан к другому пользователю")
		case errors.Is(err, auth.ErrAlreadyLinked):
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
		default:
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// 3) Неявная привязка по verified email к существующему аккаунту.
	uid, err := h.Auth.UserByEmail(r.Context(), id.Email)
	switch {
	case err == nil:
		if !id.EmailVerified {
			h.renderError(w, r, http.StatusForbidden,
				"провайдер не подтвердил email; привяжите аккаунт в профиле после входа паролем")
			return
		}
		if err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); err != nil &&
			!errors.Is(err, auth.ErrAlreadyLinked) {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		h.oauthLogin(w, r, uid, "/")
	case errors.Is(err, auth.ErrUserNotFound):
		h.oauthProvisionByInvite(w, r, name, id)
	default:
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
	}
}

// oauthProvisionByInvite заводит аккаунт по действующему инвайту на verified
// email и логинит. Нет инвайта → отказ, аккаунт не создаётся.
func (h *Handler) oauthProvisionByInvite(w http.ResponseWriter, r *http.Request, provider string, id oauth.Identity) {
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, "провайдер не подтвердил email")
		return
	}
	has, err := h.Org.HasPendingInvite(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !has {
		h.renderError(w, r, http.StatusForbidden,
			"для этого email нет приглашения — попросите администратора пригласить вас")
		return
	}
	uid, err := h.Auth.CreateOAuthUser(r.Context(), id.Email)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if _, ok, err := h.Org.AcceptPendingInviteByEmail(r.Context(), id.Email, uid); err != nil || !ok {
		// Гонка: инвайт исчез между проверкой и принятием — откатываем юзера.
		_ = h.Auth.DeleteUser(r.Context(), uid)
		h.renderError(w, r, http.StatusForbidden,
			"для этого email нет приглашения — попросите администратора пригласить вас")
		return
	}
	if err := h.Auth.LinkIdentity(r.Context(), uid, provider, id.Subject, id.Email); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.oauthLogin(w, r, uid, "/")
}

// oauthLogin выпускает сессию и редиректит.
func (h *Handler) oauthLogin(w http.ResponseWriter, r *http.Request, uid int64, dest string) {
	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
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
	h.renderError(w, r, http.StatusBadGateway, "вход через "+name+" не удался, попробуйте снова")
}

func clearOAuthCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookieName, Value: "", Path: "/auth/oauth",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
