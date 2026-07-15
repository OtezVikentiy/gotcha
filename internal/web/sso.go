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
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// ssoProviderPrefix — имя per-org SSO-провайдера в потоке /auth/oauth/{provider}:
// "sso-{orgID}". Отличает per-org OIDC (конфиг из БД) от env-провайдеров этапа 5.
const ssoProviderPrefix = "sso-"

// ssoMeta — метаданные per-org SSO для callback (domain guard + JIT-провижининг).
type ssoMeta struct {
	OrgID       int64
	Domain      string
	DefaultRole string
}

// ssoCacheTTL — как долго кешируем построенный OIDC-провайдер по orgID, чтобы не
// читать org_sso на каждый запрос (правка конфига применяется в пределах TTL).
const ssoCacheTTL = 5 * time.Minute

type ssoCacheEntry struct {
	provider *oauth.OIDC
	meta     ssoMeta
	expires  time.Time
}

// ssoCache — процесс-локальный кеш per-org OIDC-провайдеров.
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

func (c *ssoCache) put(orgID int64, p *oauth.OIDC, meta ssoMeta, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]ssoCacheEntry{}
	}
	c.entries[orgID] = ssoCacheEntry{provider: p, meta: meta, expires: now.Add(ssoCacheTTL)}
}

// resolveProvider возвращает провайдера потока OAuth по имени. Обычные
// (oidc/yandex/vk) — из env-Registry (этап 5, sso=nil). "sso-{id}" — per-org
// OIDC, построенный из org_sso (этап 10), с ssoMeta.
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

// emailDomain — часть email после '@', в нижнем регистре. Нет '@' → "".
func emailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return email[at+1:]
}

// ssoCallback — SSO-ветка callback (этап 10, JIT-провижининг вместо invite-gated).
// verified email обязателен; email обязан быть из домена организации (domain
// guard — IdP может вернуть чужой email). Новый юзер провижинится в орг с
// DefaultRole; существующий — линкуется и гарантированно становится участником.
func (h *Handler) ssoCallback(w http.ResponseWriter, r *http.Request, name string, id oauth.Identity, sso *ssoMeta) {
	if !id.EmailVerified {
		h.renderError(w, r, http.StatusForbidden, "провайдер не подтвердил email")
		return
	}
	if emailDomain(id.Email) != sso.Domain {
		h.renderError(w, r, http.StatusForbidden, "email не из домена организации")
		return
	}
	role := org.Role(sso.DefaultRole)

	// Вход по стабильному субъекту.
	if uid, err := h.Auth.IdentityUser(r.Context(), name, id.Subject); err == nil {
		_ = h.Auth.UpdateIdentityEmail(r.Context(), name, id.Subject, id.Email)
		if err := h.Org.EnsureMember(r.Context(), sso.OrgID, uid, role); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		h.oauthLogin(w, r, uid, "/")
		return
	} else if !errors.Is(err, auth.ErrNoIdentity) {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// JIT: найти/создать юзера по email, привязать identity, гарантировать членство.
	uid, err := h.Auth.UserByEmail(r.Context(), id.Email)
	switch {
	case err == nil:
		// существующий юзер
	case errors.Is(err, auth.ErrUserNotFound):
		uid, err = h.Auth.CreateOAuthUser(r.Context(), id.Email)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	default:
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.Auth.LinkIdentity(r.Context(), uid, name, id.Subject, id.Email); err != nil &&
		!errors.Is(err, auth.ErrAlreadyLinked) {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.Org.EnsureMember(r.Context(), sso.OrgID, uid, role); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.oauthLogin(w, r, uid, "/")
}
