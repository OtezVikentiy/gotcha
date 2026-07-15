package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// OIDCConfig — параметры generic-OIDC-провайдера (из env, см. cmd/gotcha/config.go).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       string // пусто → "openid email profile"
	DisplayName  string // пусто → "OIDC"
}

// OIDC — провайдер поверх OpenID Connect. discovery и JWKS кешируются на
// процесс (ленивая загрузка при первом обращении).
type OIDC struct {
	cfg OIDCConfig

	mu    sync.Mutex
	disco *discoveryDoc
	keys  []jwk
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

func (o *OIDC) scopes() string {
	if o.cfg.Scopes != "" {
		return o.cfg.Scopes
	}
	return "openid email profile"
}

// discovery лениво загружает и кеширует .well-known/openid-configuration и JWKS.
func (o *OIDC) discovery(ctx context.Context) (*discoveryDoc, []jwk, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.disco != nil {
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
	o.disco, o.keys = &doc, ks.Keys
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
		return Identity{}, err
	}
	// iss / aud / exp / nonce.
	if iss, _ := claims["iss"].(string); iss != doc.Issuer && iss != strings.TrimRight(o.cfg.Issuer, "/") {
		return Identity{}, fmt.Errorf("%w: iss mismatch", ErrBadToken)
	}
	if !audMatches(claims["aud"], o.cfg.ClientID) {
		return Identity{}, fmt.Errorf("%w: aud mismatch", ErrBadToken)
	}
	if exp, ok := claims["exp"].(float64); !ok || nowUnix() >= int64(exp) {
		return Identity{}, fmt.Errorf("%w: expired", ErrBadToken)
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
