package web

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

func TestMaskChannelTarget(t *testing.T) {
	cases := []struct{ kind, target, want string }{
		{alert.ChannelEmail, "oleg@example.com", "o***@example.com ·7953"},
		{alert.ChannelEmail, "a@b.c", "***@b.c ·d648"},
		{alert.ChannelEmail, "кириллица@почта.рф", "к***@почта.рф ·697b"},
		{alert.ChannelTelegram, "123456789", "****89 ·15e2"},
		{alert.ChannelTelegram, "9", "**** ·1958"},
		{alert.ChannelWebhook, "https://hooks.example.com/T000/B000/secret", "https://hooks.example.com/… ·e043"},
		{alert.ChannelWebhook, "https://user:pass@hooks.example.com/x", "https://hooks.example.com/… ·45f2"},
		{alert.ChannelWebhook, "не-URL", "**** ·633b"},
		{"unknown", "whatever", "**** ·8573"},
	}
	for _, c := range cases {
		if got := maskChannelTarget(c.kind, c.target); got != c.want {
			t.Errorf("maskChannelTarget(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
		}
	}
}

func TestMaskChannelTargetBoundaries(t *testing.T) {
	cases := []struct {
		name, kind, target, want string
	}{
		{"telegram 2 runes", alert.ChannelTelegram, "12", "**** ·6b51"},
		{"telegram 3 runes", alert.ChannelTelegram, "123", "****23 ·a665"},
		{"webhook with port", alert.ChannelWebhook, "https://host:8443/x", "https://host:8443/… ·84bc"},
		{"webhook IPv6 host", alert.ChannelWebhook, "https://[::1]/x", "https://[::1]/… ·11e4"},
		{"webhook host-less path parses ok", alert.ChannelWebhook, "/just/a/path", "**** ·3d13"},
		{"webhook host-less mailto parses ok", alert.ChannelWebhook, "mailto:ops@example.com", "**** ·d360"},
		{"email no at sign", alert.ChannelEmail, "not-an-email", "**** ·eba0"},
		{"email empty target", alert.ChannelEmail, "", "****"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := maskChannelTarget(c.kind, c.target)
			if got != c.want {
				t.Errorf("maskChannelTarget(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
			}
			if c.target != "" && strings.Contains(got, c.target) {
				t.Errorf("maskChannelTarget(%q, %q) leaks raw target verbatim: %q", c.kind, c.target, got)
			}
		})
	}
}

func TestMaskChannelTargetDisambiguatesSameHost(t *testing.T) {
	a := maskChannelTarget(alert.ChannelWebhook, "https://hooks.slack.com/services/T1/B1/xxx")
	b := maskChannelTarget(alert.ChannelWebhook, "https://hooks.slack.com/services/T2/B2/yyy")
	if a == b {
		t.Fatalf("two distinct same-host webhook targets masked identically: %q", a)
	}
	if a[:len("https://hooks.slack.com/…")] != "https://hooks.slack.com/…" ||
		b[:len("https://hooks.slack.com/…")] != "https://hooks.slack.com/…" {
		t.Fatalf("unexpected mask prefix: %q / %q", a, b)
	}
	if a != "https://hooks.slack.com/… ·4c65" {
		t.Errorf("target 1: got %q, want %q", a, "https://hooks.slack.com/… ·4c65")
	}
	if b != "https://hooks.slack.com/… ·c432" {
		t.Errorf("target 2: got %q, want %q", b, "https://hooks.slack.com/… ·c432")
	}
}

func TestAlertDeliveriesRedactionOrderMatters(t *testing.T) {
	const rawTarget = "https://hooks.example.com/T000/B000/order-pin-secret"
	lastErr := "upstream rejected POST to " + rawTarget + ": connection reset"

	redacted := notify.RedactToken(lastErr, rawTarget)
	if strings.Contains(redacted, rawTarget) {
		t.Fatalf("correct order (redact-then-mask) left target in LastError: %q", redacted)
	}

	masked := maskChannelTarget(alert.ChannelWebhook, rawTarget)
	reversed := notify.RedactToken(lastErr, masked)
	if !strings.Contains(reversed, rawTarget) {
		t.Fatalf("test invalid: reversed order (mask-then-redact) unexpectedly stripped the target too — order is no longer distinguishable by this test, adjust fixtures")
	}
}

func TestMaskDiscriminatorDeterministicAndOneWay(t *testing.T) {
	got1 := maskDiscriminator("https://hooks.example.com/T000/B000/secret-abc")
	got2 := maskDiscriminator("https://hooks.example.com/T000/B000/secret-abc")
	if got1 != got2 {
		t.Fatalf("maskDiscriminator not deterministic: %q != %q", got1, got2)
	}
	if len(got1) != len("·")+4 {
		t.Errorf("maskDiscriminator length = %d, want %d (·+4 hex)", len(got1), len("·")+4)
	}
	if strings.Contains(got1, "secret-abc") {
		t.Errorf("maskDiscriminator leaked raw target: %q", got1)
	}
}
