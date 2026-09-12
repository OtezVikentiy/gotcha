package main

import "gitflic.ru/otezvikentiy/gotcha/internal/oauth"

// Порядок провайдеров — порядок кнопок на /login.
func buildRegistry(cfg Config) *oauth.Registry {
	var providers []oauth.Provider
	if cfg.OIDCEnabled {
		providers = append(providers, oauth.NewOIDC(oauth.OIDCConfig{
			Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret, Scopes: cfg.OIDCScopes, DisplayName: cfg.OIDCName,
		}))
	}
	if cfg.YandexEnabled {
		providers = append(providers, oauth.NewYandex(oauth.YandexConfig{
			ClientID: cfg.YandexClientID, ClientSecret: cfg.YandexClientSecret,
		}))
	}
	if cfg.VKEnabled {
		providers = append(providers, oauth.NewVK(oauth.VKConfig{
			ClientID: cfg.VKClientID, ClientSecret: cfg.VKClientSecret,
		}))
	}
	return oauth.NewRegistry(providers...)
}
