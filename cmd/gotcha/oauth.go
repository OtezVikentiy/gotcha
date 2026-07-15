package main

import "gitflic.ru/otezvikentiy/gotcha/internal/oauth"

// buildRegistry собирает включённые OAuth-провайдеры в фиксированном порядке
// (OIDC, Яндекс, VK — порядок кнопок на /login). Валидация обязательных полей
// уже сделана в loadConfig, поэтому здесь просто конструируем.
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
