package web

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// TestMaskChannelTarget — таблица масок цели канала для не-админа (спека
// 2026-08-08, решения владельца 2026-08-10): email — первая руна +
// ***@домен; telegram — последние 2 цифры chat_id; webhook — только
// scheme://host, путь и query часто несут токены; неразбираемое — "****".
func TestMaskChannelTarget(t *testing.T) {
	cases := []struct{ kind, target, want string }{
		{alert.ChannelEmail, "oleg@example.com", "o***@example.com"},
		{alert.ChannelEmail, "a@b.c", "***@b.c"}, // локальная часть из 1 руны не палится
		{alert.ChannelEmail, "кириллица@почта.рф", "к***@почта.рф"},
		{alert.ChannelTelegram, "123456789", "****89"},
		{alert.ChannelTelegram, "9", "****"}, // короче 3 рун — целиком
		{alert.ChannelWebhook, "https://hooks.example.com/T000/B000/secret", "https://hooks.example.com/…"},
		{alert.ChannelWebhook, "https://user:pass@hooks.example.com/x", "https://hooks.example.com/…"}, // userinfo не в u.Host — не палится, но и не выводится
		{alert.ChannelWebhook, "не-URL", "****"},
		{"unknown", "whatever", "****"},
	}
	for _, c := range cases {
		if got := maskChannelTarget(c.kind, c.target); got != c.want {
			t.Errorf("maskChannelTarget(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
		}
	}
}
