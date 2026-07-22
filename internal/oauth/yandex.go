package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// YandexConfig — параметры Яндекс ID (из env).
type YandexConfig struct {
	ClientID     string
	ClientSecret string
}

// Yandex — провайдер Яндекс ID (OAuth2, свой профиль login.yandex.ru/info).
// Не чистый OIDC: id_token не используется, email считаем verified (Яндекс
// отдаёт лишь собственный подтверждённый адрес аккаунта).
type Yandex struct {
	cfg YandexConfig

	authorizeURL string
	tokenURL     string
	infoURL      string
}

func NewYandex(cfg YandexConfig) *Yandex {
	return &Yandex{
		cfg:          cfg,
		authorizeURL: "https://oauth.yandex.ru/authorize",
		tokenURL:     "https://oauth.yandex.ru/token",
		infoURL:      "https://login.yandex.ru/info?format=json",
	}
}

func (y *Yandex) Name() string        { return "yandex" }
func (y *Yandex) DisplayName() string { return "Яндекс" }

// AuthURL — Яндекс не требует PKCE; challenge игнорируем. nonce не применим.
func (y *Yandex) AuthURL(state, _, _, redirectURI string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {y.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"state":         {state},
	}
	return y.authorizeURL + "?" + q.Encode()
}

func (y *Yandex) Exchange(ctx context.Context, code, _, redirectURI, _ string) (Identity, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {y.cfg.ClientID},
		"client_secret": {y.cfg.ClientSecret},
		"redirect_uri":  {redirectURI},
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, y.tokenURL, form, &tok); err != nil {
		return Identity{}, fmt.Errorf("%w: yandex token: %v", ErrExchange, err)
	}
	if tok.AccessToken == "" {
		return Identity{}, fmt.Errorf("%w: yandex no access_token", ErrExchange)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, y.infoURL, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: yandex info req: %v", ErrExchange, err)
	}
	req.Header.Set("Authorization", "OAuth "+tok.AccessToken)
	resp, err := sharedClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: yandex info: %v", ErrExchange, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("%w: yandex info status %d", ErrExchange, resp.StatusCode)
	}
	var info struct {
		ID           string `json:"id"`
		DefaultEmail string `json:"default_email"`
		DisplayName  string `json:"display_name"`
	}
	if err := decodeJSON(resp.Body, &info); err != nil {
		return Identity{}, fmt.Errorf("%w: yandex info decode: %v", ErrExchange, err)
	}
	if info.DefaultEmail == "" {
		return Identity{}, ErrNoEmail
	}
	return Identity{
		Subject:       info.ID,
		Email:         info.DefaultEmail,
		EmailVerified: true,
		TrustedIssuer: true,
		DisplayName:   info.DisplayName,
	}, nil
}
