package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

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

func TestChannelStatusBadgeTrusted(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantText := i18n.T(ctx, "alerts.channels.status.trusted")
	out := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true, Trusted: true}))
	if !strings.Contains(out, wantText) {
		t.Fatalf("Trusted должен показывать доп. бейдж %q: %s", wantText, out)
	}
}

func TestChannelStatusBadgeNotTrusted(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	trustedText := i18n.T(ctx, "alerts.channels.status.trusted")
	out := renderTo(t, channelStatusBadge(alert.Channel{Enabled: true}))
	if strings.Contains(out, trustedText) {
		t.Fatalf("без Trusted бейдж %q не должен появляться: %s", trustedText, out)
	}
}
