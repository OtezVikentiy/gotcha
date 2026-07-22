// Package oauth — вход через внешних провайдеров идентичности (social login):
// generic OIDC, Яндекс ID, VK ID. Провайдеры настраиваются на инсталляцию
// через env и собираются в Registry; web-слой (internal/web) водит поток
// authorize→callback и решает провижининг (link-only/invite-gated).
package oauth

import (
	"context"
	"errors"
)

var (
	// ErrNoEmail — провайдер не отдал email (вход невозможен: аккаунты в
	// Gotcha идентифицируются по email).
	ErrNoEmail = errors.New("oauth: provider returned no email")
	// ErrExchange — обмен кода на токен/профиль не удался (сеть, невалидный
	// ответ провайдера). Наружу отдаём нейтральную страницу.
	ErrExchange = errors.New("oauth: token exchange failed")
)

// Identity — то, что провайдер знает о вошедшем пользователе.
type Identity struct {
	Subject       string // стабильный id пользователя у провайдера
	Email         string
	EmailVerified bool
	// TrustedIssuer — провайдер сам по себе доверенный источник верификации
	// email (курируемая интеграция: VK/Яндекс подтверждают владение адресом).
	// Только тогда EmailVerified можно доверять для НЕЯВНОЙ привязки к уже
	// существующему аккаунту с тем же email. У generic-OIDC false: email_verified
	// приходит от произвольного IdP и им нельзя доверять для auto-link (иначе
	// IdP, заявивший чужой адрес, угнал бы существующий парольный аккаунт).
	TrustedIssuer bool
	DisplayName   string
}

// Provider — один внешний провайдер. AuthURL строит ссылку на страницу
// согласия; Exchange меняет код авторизации на Identity.
type Provider interface {
	Name() string        // 'oidc' | 'yandex' | 'vk' — стабильный ключ (колонка provider)
	DisplayName() string // подпись кнопки, напр. "Яндекс"
	AuthURL(state, nonce, pkceChallenge, redirectURI string) string
	Exchange(ctx context.Context, code, pkceVerifier, redirectURI, nonce string) (Identity, error)
}
