package web

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
)

type labelProvider struct{ name, display string }

func (p labelProvider) Name() string                     { return p.name }
func (p labelProvider) DisplayName() string              { return p.display }
func (p labelProvider) AuthURL(_, _, _, _ string) string { return "https://idp/a" }
func (p labelProvider) Exchange(_ context.Context, _, _, _, _ string) (oauth.Identity, error) {
	return oauth.Identity{}, nil
}

// TestProviderLabelLocale — подпись OAuth-провайдера локализуется по ключу
// oauth.provider.<name> (№137): на en Яндекс подписан "Yandex" и кириллицы на
// кнопке нет; generic OIDC с произвольным именем (ключа в каталоге нет)
// показывает DisplayName как есть на любой локали.
func TestProviderLabelLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})

	if got := providerLabel(ru, "yandex", "Yandex"); got != "Яндекс" {
		t.Errorf("ru yandex = %q, want Яндекс", got)
	}
	if got := providerLabel(en, "yandex", "Yandex"); got != "Yandex" {
		t.Errorf("en yandex = %q, want Yandex", got)
	}
	if got := providerLabel(en, "corp-sso", "Наш SSO"); got != "Наш SSO" {
		t.Errorf("generic = %q, want DisplayName как есть", got)
	}

	h := New(nil, nil, nil, nil, "http://localhost:8080")
	h.OAuth = oauth.NewRegistry(
		labelProvider{name: "yandex", display: "Yandex"},
		labelProvider{name: "corp-sso", display: "Corp SSO"},
	)
	for _, b := range h.oauthButtons(en) {
		if strings.ContainsFunc(b.Label, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
			t.Errorf("кириллица в подписи кнопки на en-локали: %q", b.Label)
		}
	}
	buttons := h.oauthButtons(ru)
	if len(buttons) != 2 || !strings.Contains(buttons[0].Label, "Яндекс") {
		t.Errorf("ru кнопки = %+v, ждали «Яндекс» в первой", buttons)
	}
}
