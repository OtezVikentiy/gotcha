package oauth

import (
	"context"
	"errors"
)

var (
	ErrNoEmail  = errors.New("oauth: provider returned no email")
	ErrExchange = errors.New("oauth: token exchange failed")
)

type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	TrustedIssuer bool
	DisplayName   string
}

type Provider interface {
	Name() string
	DisplayName() string
	AuthURL(state, nonce, pkceChallenge, redirectURI string) string
	Exchange(ctx context.Context, code, pkceVerifier, redirectURI, nonce string) (Identity, error)
}
