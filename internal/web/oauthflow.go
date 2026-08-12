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
		h.oauthFail(w, r, name, p)
		return
	}
	id, err := p.Exchange(r.Context(), code, flow.Verifier, h.oauthRedirectURI(name), flow.Nonce)
	if err != nil || id.Subject == "" || id.Email == "" {
		slog.Warn("oauth exchange failed", "provider", name, "err", err)
		h.oauthFail(w, r, name, p)
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
	enforced, err := h.enforcedSSO(r.Context(), emailDomain(id.Email))
	if err != nil {
		// Fail closed: гейт не смог ответить — уводим на SSO, а не пропускаем
		// мимо централизованного provisioning.
		slog.Error("oauth: enforced SSO lookup failed", "error", err)
		http.Redirect(w, r, "/sso", http.StatusSeeOther)
		return
	}
	if enforced {
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

	// 2) Поток привязки из профиля. Требуем СОВПАДЕНИЯ двух независимых источников:
	// UID в подписанной cookie (кто начинал поток) и UID текущей сессии (кто
	// завершает). Каждый по отдельности недостаточен:
	//   - только сессия (как было) — захват аккаунта подменой cookie: атакующий
	//     начинает свой link-поток, проходит IdP, подсовывает жертве свою cookie
	//     потока, и на её callback ЕГО identity привязывается к ЕЁ аккаунту, после
	//     чего он входит под жертвой;
	//   - только cookie — при утёкшем ключе подписи UID подделывается (SEC-C1).
	// Совпадение закрывает оба: чужая cookie несёт чужой UID, а подделанный UID не
	// совпадёт с сессией атакующего.
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
		h.oauthProvision(w, r, name, id)
	default:
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// oauthProvision заводит аккаунт по OAuth-входу согласно режиму регистрации и
// логинит (№96). closed — отказ всегда; invite — только по действующему
// приглашению на verified email; open — аккаунт создаётся без приглашения
// (симметрично парольной open-регистрации), действующее приглашение
// принимается best-effort.
func (h *Handler) oauthProvision(w http.ResponseWriter, r *http.Request, provider string, id oauth.Identity) {
	// closed — «регистрация выключена полностью»: новых аккаунтов не появляется
	// ВООБЩЕ, даже по действующему приглашению. Именно этим closed отличается от
	// invite: раньше оба режима проверялись как `!= "open"`, приглашения работали
	// в обоих, и различия между ними на деле не существовало — документация
	// обещала то, чего код не делал. Приглашение УЖЕ существующего пользователя в
	// организацию closed не трогает: это членство, а не регистрация.
	if h.RegistrationMode == "closed" {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.no_invite"))
		return
	}
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.provider_no_email"))
		return
	}
	// open — регистрация открыта всем: аккаунт заводится без проверки
	// приглашения, как и в парольном флоу (auth.go: open пропускает без
	// токена). Действующее приглашение при этом принимается сразу — адрес
	// подтверждён провайдером, как и в invite-ветке ниже. Неудача принятия
	// (приглашения нет, гонка, ошибка БД) аккаунт НЕ откатывает: парольная
	// open-регистрация членство тоже не выдаёт, вход важнее членства.
	if h.RegistrationMode == "open" {
		uid, err := h.Auth.CreateOAuthUser(r.Context(), id.Email)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if _, _, err := h.Org.AcceptPendingInviteByEmail(r.Context(), id.Email, uid); err != nil {
			slog.Warn("oauth open provisioning: accept pending invite", "err", err)
		}
		if err := h.Auth.LinkIdentity(r.Context(), uid, provider, id.Subject, id.Email); err != nil {
			// uid только что создан ЭТИМ вызовом (CreateOAuthUser выше) — без
			// LinkIdentity он никому не принадлежит: занимает email, войти под
			// ним нельзя ни паролем (OAuth-юзер его не получает), ни этим же
			// провайдером (identity не привязана). Откатываем best-effort, как
			// invite-ветка уже делает при сбое AcceptPendingInviteByEmail.
			_ = h.Auth.DeleteUser(r.Context(), uid)
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.oauthLogin(w, r, uid, "/")
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
		// Тот же откат, что и открытая ветка выше, и по той же причине: uid
		// создан этим самым вызовом, LinkIdentity — последний шаг, без него
		// аккаунт занимает email и недоступен ни одним способом входа.
		_ = h.Auth.DeleteUser(r.Context(), uid)
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
// p — уже резолвленный resolveProvider провайдер (см. вызовы в oauthCallback):
// для обычных (env) провайдеров это то же, что вернул бы h.OAuth.Get, но для
// per-org SSO ("sso-{id}") h.OAuth.Get не найдёт ничего — раньше это оставляло
// пользователю сырое внутреннее имя провайдера ("sso-42") вместо названия
// организации (DisplayName у per-org OIDC — cfg.Domain, см. resolveProvider).
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
