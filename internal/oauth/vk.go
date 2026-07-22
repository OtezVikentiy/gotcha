package oauth

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// VKConfig — параметры VK ID (из env).
type VKConfig struct {
	ClientID     string
	ClientSecret string
}

// VK — провайдер VK ID (OAuth2). Особенность: email и user_id приходят в
// token-ответе, а не в профиле; имя добираем через users.get. Email считаем
// verified. Если VK не вернул email (юзер не выдал доступ) → ErrNoEmail.
type VK struct {
	cfg VKConfig

	authorizeURL string
	tokenURL     string
	usersURL     string
}

func NewVK(cfg VKConfig) *VK {
	return &VK{
		cfg:          cfg,
		authorizeURL: "https://oauth.vk.com/authorize",
		tokenURL:     "https://oauth.vk.com/access_token",
		usersURL:     "https://api.vk.com/method/users.get",
	}
}

func (v *VK) Name() string        { return "vk" }
func (v *VK) DisplayName() string { return "VK" }

func (v *VK) AuthURL(state, _, _, redirectURI string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {v.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"scope":         {"email"},
		"state":         {state},
	}
	return v.authorizeURL + "?" + q.Encode()
}

func (v *VK) Exchange(ctx context.Context, code, _, redirectURI, _ string) (Identity, error) {
	form := url.Values{
		"client_id":     {v.cfg.ClientID},
		"client_secret": {v.cfg.ClientSecret},
		"redirect_uri":  {redirectURI},
		"code":          {code},
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		UserID      int64  `json:"user_id"`
		Email       string `json:"email"`
	}
	if err := postForm(ctx, v.tokenURL, form, &tok); err != nil {
		return Identity{}, fmt.Errorf("%w: vk token: %v", ErrExchange, err)
	}
	if tok.AccessToken == "" || tok.UserID == 0 {
		return Identity{}, fmt.Errorf("%w: vk bad token response", ErrExchange)
	}
	if tok.Email == "" {
		return Identity{}, ErrNoEmail
	}
	id := Identity{
		Subject:       strconv.FormatInt(tok.UserID, 10),
		Email:         tok.Email,
		EmailVerified: true,
		TrustedIssuer: true,
	}
	// Имя — best-effort через users.get; ошибка не критична для входа.
	q := url.Values{
		"user_ids":     {id.Subject},
		"access_token": {tok.AccessToken},
		"v":            {"5.199"},
	}
	var profile struct {
		Response []struct {
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"response"`
	}
	if err := getJSON(ctx, v.usersURL+"?"+q.Encode(), &profile); err == nil && len(profile.Response) > 0 {
		id.DisplayName = strings.TrimSpace(profile.Response[0].FirstName + " " + profile.Response[0].LastName)
	}
	return id, nil
}
