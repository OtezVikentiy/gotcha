package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestChannelStatusBadgeSecretBroken — секрет канала не расшифровывается
// (сменился/потерян GOTCHA_SECRET_KEY): бейдж состояния — danger с текстом
// secret_broken, а не enabled/disabled. Существующий тест в labels_test.go
// покрывает только Enabled true/false — ветка SecretBroken не исполнялась.
func TestChannelStatusBadgeSecretBroken(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantText := i18n.T(ctx, "alerts.channels.status.secret_broken")
	out := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true, SecretBroken: true}))
	if !strings.Contains(out, wantText) {
		t.Fatalf("SecretBroken должен показывать %q даже при Enabled=true: %s", wantText, out)
	}
	if !strings.Contains(out, "badge-danger") {
		t.Fatalf("SecretBroken должен рисовать badge-danger: %s", out)
	}
}

// TestChannelStatusBadgeTrusted — канал, помеченный доверенным (получатель
// внутри контура): второй бейдж "trusted" рядом со статусом.
func TestChannelStatusBadgeTrusted(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantText := i18n.T(ctx, "alerts.channels.status.trusted")
	out := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true, Trusted: true}))
	if !strings.Contains(out, wantText) {
		t.Fatalf("Trusted должен показывать доп. бейдж %q: %s", wantText, out)
	}
}

// TestChannelStatusBadgeNotTrusted — обратная ветка: без Trusted второй
// бейдж не рисуется.
func TestChannelStatusBadgeNotTrusted(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	trustedText := i18n.T(ctx, "alerts.channels.status.trusted")
	out := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true}))
	if strings.Contains(out, trustedText) {
		t.Fatalf("без Trusted бейдж %q не должен появляться: %s", trustedText, out)
	}
}
