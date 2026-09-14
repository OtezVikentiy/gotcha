package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// Отличает per-org OIDC (конфиг из БД) от env-провайдеров.
const ssoProviderPrefix = "sso-"

type ssoMeta struct {
	OrgID       int64
	Domain      string
	DefaultRole string
}

// Не читаем org_sso на каждый запрос — правка конфига применяется в пределах TTL.
const ssoCacheTTL = 5 * time.Minute

type ssoCacheEntry struct {
	provider *oauth.OIDC
	meta     ssoMeta
	expires  time.Time
}

type ssoCache struct {
	mu      sync.Mutex
	entries map[int64]ssoCacheEntry
}

func (c *ssoCache) get(orgID int64, now time.Time) (*oauth.OIDC, ssoMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[orgID]; ok && e.expires.After(now) {
		return e.provider, e.meta, true
	}
	return nil, ssoMeta{}, false
}

// Без вызова при изменении/удалении федерации отозванный IdP продолжал бы выдавать логины —
// отзыв (например, при компрометации IdP) применялся бы с задержкой до ssoCacheTTL.
func (c *ssoCache) invalidate(orgID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, orgID)
}

func (c *ssoCache) put(orgID int64, p *oauth.OIDC, meta ssoMeta, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]ssoCacheEntry{}
	}
	c.entries[orgID] = ssoCacheEntry{provider: p, meta: meta, expires: now.Add(ssoCacheTTL)}
}

func (h *Handler) resolveProvider(ctx context.Context, name string) (oauth.Provider, *ssoMeta, bool) {
	if !strings.HasPrefix(name, ssoProviderPrefix) {
		p, ok := h.OAuth.Get(name)
		return p, nil, ok
	}
	orgID, err := strconv.ParseInt(strings.TrimPrefix(name, ssoProviderPrefix), 10, 64)
	if err != nil || h.Org == nil {
		return nil, nil, false
	}
	now := time.Now()
	if p, meta, ok := h.ssoProviders.get(orgID, now); ok {
		return p, &meta, true
	}
	cfg, ok, err := h.Org.SSOByOrg(ctx, orgID)
	if err != nil || !ok {
		return nil, nil, false
	}
	p := oauth.NewOIDC(oauth.OIDCConfig{
		Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		Scopes: "openid email profile", DisplayName: cfg.Domain,
	})
	meta := ssoMeta{OrgID: orgID, Domain: cfg.Domain, DefaultRole: cfg.DefaultRole}
	h.ssoProviders.put(orgID, p, meta, now)
	return p, &meta, true
}

// Обрезаем ВСЕ конечные точки FQDN («user@enforced.com..» → «enforced.com») через TrimRight,
// не TrimSuffix — иначе trailing-dot обходил бы enforced-SSO гейт и domain guard.
func emailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.TrimRight(email[at+1:], ".")
	return domain
}

// Ошибка возвращается наружу, не сворачивается в false: иначе смена GOTCHA_SECRET_KEY или сбой
// pgx молча снимает обязательный SSO. Падать в 500 дешевле, чем пустить.
func (h *Handler) enforcedSSO(ctx context.Context, domain string) (bool, error) {
	if h.Org == nil || domain == "" {
		return false, nil
	}
	cfg, ok, err := h.Org.SSOByDomain(ctx, domain)
	if err != nil {
		return false, err
	}
	return ok && cfg.Enforced, nil
}

// verified email обязателен, домен обязан совпасть с sso.Domain. Существующий аккаунт с этим
// email НЕ линкуется по одному совпадению — только с активной сессией (flow.Link), см. ниже.
func (h *Handler) ssoCallback(w http.ResponseWriter, r *http.Request, name string, id oauth.Identity, sso *ssoMeta, flow oauthFlow) {
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.provider_no_email"))
		return
	}
	if emailDomain(id.Email) != sso.Domain {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.email_not_in_domain"))
		return
	}
	role := org.Role(sso.DefaultRole)

	// Резолвится один раз для ОБЕИХ веток ниже (идентичность уже есть / её нет): иначе
	// ветка «идентичность уже есть» логинила бы вызывающего под ЧУЖИМ uid, если чужая
	// идентичность случайно (или намеренно, при контроле над IdP) всплыла в потоке привязки.
	var sessionUID int64
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
		sessionUID = uid
	}

	if uid, err := h.Auth.IdentityUser(r.Context(), name, id.Subject); err == nil {
		if flow.Link && uid != sessionUID {
			h.renderError(w, r, http.StatusConflict,
				i18n.Tf(r.Context(), "error.oauth.already_linked", "provider", providerLabel(r.Context(), name, sso.Domain)))
			return
		}
		_ = h.Auth.UpdateIdentityEmail(r.Context(), name, id.Subject, id.Email)
		if err := h.Org.EnsureMember(r.Context(), sso.OrgID, uid, role); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if flow.Link {
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
			return
		}
		h.oauthLogin(w, r, uid, "/")
		return
	} else if !errors.Is(err, auth.ErrNoIdentity) {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	if flow.Link {
		switch err := h.Auth.LinkIdentity(r.Context(), sessionUID, name, id.Subject, id.Email); {
		case err == nil, errors.Is(err, auth.ErrAlreadyLinked):
		case errors.Is(err, auth.ErrIdentityTaken):
			h.renderError(w, r, http.StatusConflict,
				i18n.Tf(r.Context(), "error.oauth.already_linked", "provider", providerLabel(r.Context(), name, sso.Domain)))
			return
		default:
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if err := h.Org.EnsureMember(r.Context(), sso.OrgID, sessionUID, role); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	uid, err := h.Auth.UserByEmail(r.Context(), id.Email)
	switch {
	case err == nil:
		// Тот же захват, что generic-OIDC запрещает через TrustedIssuer: аккаунт уже есть,
		// линковать его без активной сессии владельца нельзя.
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.oauth.sso_account_exists"))
		return
	case errors.Is(err, auth.ErrUserNotFound):
		uid, err = h.Auth.CreateOAuthUser(r.Context(), id.Email)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); err != nil &&
		!errors.Is(err, auth.ErrAlreadyLinked) {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.Org.EnsureMember(r.Context(), sso.OrgID, uid, role); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.oauthLogin(w, r, uid, "/")
}
