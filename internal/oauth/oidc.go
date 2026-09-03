package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCConfig — параметры generic-OIDC-провайдера (из env, см. cmd/gotcha/config.go).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       string // пусто → "openid email profile"
	DisplayName  string // пусто → "OIDC"
}

// discoveryTTL — срок жизни кеша discovery/JWKS. Раньше кеш жил ВЕСЬ процесс:
// после плановой ротации подписного ключа у IdP (рутина для Keycloak/Auth0/Azure)
// verifyRS256 переставал находить ключ, и КАЖДЫЙ OIDC-вход падал до перезапуска
// бинаря — причём диагностировалось плохо: пользователь нормально доходил до IdP
// и видел общую ошибку на callback.
const discoveryTTL = 15 * time.Minute

// OIDC — провайдер поверх OpenID Connect. discovery и JWKS кешируются на
// discoveryTTL (ленивая загрузка при первом обращении).
type OIDC struct {
	cfg OIDCConfig

	mu        sync.Mutex
	disco     *discoveryDoc
	keys      []jwk
	fetchedAt time.Time
	// now — часы (тесты перематывают время без ожидания). nil → time.Now.
	now func() time.Time
}

func (o *OIDC) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

func NewOIDC(cfg OIDCConfig) *OIDC { return &OIDC{cfg: cfg} }

func (o *OIDC) Name() string { return "oidc" }

func (o *OIDC) DisplayName() string {
	if o.cfg.DisplayName != "" {
		return o.cfg.DisplayName
	}
	return "OIDC"
}

// scopes — список scope, отправляемый в authorization-запросе. Пусто (или
// разбор ниже не оставил ни одного элемента) → дефолт "openid email profile".
//
// GOTCHA_OIDC_SCOPES принимает список через запятую — тот же разделитель,
// что у всех остальных списочных переменных контракта (GOTCHA_SCRUB_DENY_KEYS,
// GOTCHA_TRUSTED_RECIPIENTS, GOTCHA_TRUSTED_PROXIES в cmd/gotcha/config.go).
// Раньше значение уходило в scope= как есть: разделителем там пробел (см.
// RFC 6749 §3.3), и оператор, написавший по аналогии с остальными списками
// "openid,email,profile", получал ОДИН scope из трёх слов, слипшихся через
// запятую, — провайдер такой не знает, и вход отваливается на первом же
// логине без единой подсказки, где искать причину. Здесь запятая разбирается
// и нормализуется в пробел непосредственно перед отправкой; пустые элементы
// (лишняя/двойная запятая) отбрасываются, каждый — триммится.
func (o *OIDC) scopes() string {
	var parts []string
	for _, s := range strings.Split(o.cfg.Scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "openid email profile"
	}
	return strings.Join(parts, " ")
}

// discovery лениво загружает и кеширует .well-known/openid-configuration и JWKS
// на discoveryTTL. force=true перезагружает независимо от возраста кеша — так
// закрывается ротация ключа между обновлениями (см. Exchange).
func (o *OIDC) discovery(ctx context.Context) (*discoveryDoc, []jwk, error) {
	return o.discoveryFresh(ctx, false)
}

func (o *OIDC) discoveryFresh(ctx context.Context, force bool) (*discoveryDoc, []jwk, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.disco != nil && !force && o.clock().Sub(o.fetchedAt) < discoveryTTL {
		return o.disco, o.keys, nil
	}
	docURL := strings.TrimRight(o.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var doc discoveryDoc
	if err := getJSON(ctx, docURL, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: discovery: %v", ErrExchange, err)
	}
	var ks jwks
	if err := getJSON(ctx, doc.JWKSURI, &ks); err != nil {
		return nil, nil, fmt.Errorf("%w: jwks: %v", ErrExchange, err)
	}
	o.disco, o.keys, o.fetchedAt = &doc, ks.Keys, o.clock()
	return o.disco, o.keys, nil
}

// AuthURL — ссылка на страницу согласия. Для построения нужен только
// authorization_endpoint из discovery; если discovery ещё не загружен, грузим.
func (o *OIDC) AuthURL(state, nonce, pkceChallenge, redirectURI string) string {
	doc, _, err := o.discovery(context.Background())
	if err != nil {
		// AuthURL не возвращает ошибку по контракту Provider; при недоступном
		// issuer вернём пустую строку — вызывающий (web) обработает как отказ.
		return ""
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {o.scopes()},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {pkceChallenge},
		"code_challenge_method": {"S256"},
	}
	return doc.AuthorizationEndpoint + "?" + q.Encode()
}

// Exchange меняет код на id_token, валидирует его (подпись, iss, aud, exp,
// nonce) и извлекает Identity. Email/verified — из claims, при отсутствии
// email добираем userinfo. Пустой email → ErrNoEmail.
func (o *OIDC) Exchange(ctx context.Context, code, pkceVerifier, redirectURI, nonce string) (Identity, error) {
	doc, keys, err := o.discovery(ctx)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {o.cfg.ClientID},
		"client_secret": {o.cfg.ClientSecret},
		"code_verifier": {pkceVerifier},
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, doc.TokenEndpoint, form, &tok); err != nil {
		return Identity{}, fmt.Errorf("%w: token: %v", ErrExchange, err)
	}
	if tok.IDToken == "" {
		return Identity{}, fmt.Errorf("%w: no id_token", ErrExchange)
	}
	claims, err := verifyRS256(tok.IDToken, keys)
	if err != nil {
		// Подпись не сошлась ни с одним известным ключом — возможно, IdP только что
		// ротировал ключ, а наш кеш ещё свеж. Обновляем принудительно ОДИН раз и
		// пробуем снова, иначе вход был бы сломан до истечения discoveryTTL.
		_, freshKeys, ferr := o.discoveryFresh(ctx, true)
		if ferr != nil {
			return Identity{}, err
		}
		claims, err = verifyRS256(tok.IDToken, freshKeys)
		if err != nil {
			return Identity{}, err
		}
	}
	// iss / aud / exp / nonce.
	if iss, _ := claims["iss"].(string); iss != doc.Issuer && iss != strings.TrimRight(o.cfg.Issuer, "/") {
		return Identity{}, fmt.Errorf("%w: iss mismatch", ErrBadToken)
	}
	if !audMatches(claims["aud"], o.cfg.ClientID) {
		return Identity{}, fmt.Errorf("%w: aud mismatch", ErrBadToken)
	}
	if exp, ok := claims["exp"].(float64); !ok || nowUnix() >= int64(exp)+clockSkewLeeway {
		return Identity{}, fmt.Errorf("%w: expired", ErrBadToken)
	}
	// nbf (not before), если задан — с тем же допуском на дрейф часов.
	if nbfRaw, ok := claims["nbf"]; ok {
		if nbf, ok := nbfRaw.(float64); ok && nowUnix()+clockSkewLeeway < int64(nbf) {
			return Identity{}, fmt.Errorf("%w: token not yet valid (nbf)", ErrBadToken)
		}
	}
	if n, _ := claims["nonce"].(string); n != nonce {
		return Identity{}, fmt.Errorf("%w: nonce mismatch", ErrBadToken)
	}

	id := Identity{
		Subject:       asString(claims["sub"]),
		Email:         asString(claims["email"]),
		EmailVerified: claims["email_verified"] == true,
		DisplayName:   asString(claims["name"]),
	}
	if id.Subject == "" {
		return Identity{}, fmt.Errorf("%w: no sub", ErrBadToken)
	}
	if id.Email == "" && doc.UserinfoEndpoint != "" && tok.AccessToken != "" {
		if ui, err := o.userinfo(ctx, doc.UserinfoEndpoint, tok.AccessToken); err == nil {
			id.Email = asString(ui["email"])
			if v, ok := ui["email_verified"].(bool); ok {
				id.EmailVerified = v
			}
		}
	}
	if id.Email == "" {
		return Identity{}, ErrNoEmail
	}
	return id, nil
}

func (o *OIDC) userinfo(ctx context.Context, endpoint, accessToken string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := sharedClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var m map[string]any
	if err := decodeJSON(resp.Body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func audMatches(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
