package templates

import "testing"

func TestSecretLikeSegment(t *testing.T) {
	cases := []struct {
		name, seg  string
		last, want bool
	}{
		{"empty", "", false, false},
		{"dictionary word", "hooks", false, false},
		{"dictionary word long", "incidents", false, false},
		{"api version", "v1", false, false},
		{"short id", "T000", false, false},
		{"slack team id 9 alnum", "T024BE7LD", false, false},
		{"10 alnum is secret", "1234cb88b0", false, true},
		{"hex token 16", "a4d718d555cb88b0", false, true},
		{"letters-only 11 not secret", "xsecrettail", false, false},
		{"letters-only cyrillic 13 not secret", "секретныйпуть", false, false},
		{"discord snowflake 19 digits", "1234567890123456789", false, false},
		{"20 digits is secret", "12345678901234567890", false, true},
		{"long cyrillic 25 is secret", "оченьдлинныйсекретныйпуть", false, true},
		{"cyrillic letters and digits 11", "пароль12345", false, true},
		{"short mixed 6 non-last", "o2eyvv", false, false},
		{"last mixed 6", "o2eyvv", true, true},
		{"last mixed 5 below threshold", "o2eyv", true, false},
		{"last mixed 7", "o2eyvv7", true, true},
		{"last letters-only word", "incidents", true, false},
		{"last letters-only 11", "xsecrettail", true, false},
		{"last digits 6 masked", "123456", true, true},
		{"last digits 5 shown", "12345", true, false},
		{"already masked last", "… ·ab12", true, false},
		{"already masked bare ellipsis", "…", true, false},
	}
	for _, c := range cases {
		if got := secretLikeSegment(c.seg, c.last); got != c.want {
			t.Errorf("%s: secretLikeSegment(%q, last=%v) = %v, want %v", c.name, c.seg, c.last, got, c.want)
		}
	}
}

func TestMaskedWebhookTarget(t *testing.T) {
	// хосты вендоров — зарезервированный TLD .example (RFC 2606): реальные роняли push protection GitHub.
	// хост входит в длину результата (>60 рун сворачивает путь) — не удлинять, максимум сейчас 54 руны.
	cases := []struct{ name, target, want string }{
		{"secret in path", "https://hooks.example.com/services/T000/B000/1234cb88b0", "hooks.example.com/services/T000/B000/…88b0"},
		{"slack-like path", "https://hooks.slack.example/services/T024BE7LD/B024BE7LD/a4d718d555cb88b0aabbccdd", "hooks.slack.example/services/T024BE7LD/B024BE7LD/…ccdd"},
		{"mattermost-like key", "https://mm.example.com/hooks/hup3y9mggjrr8r9nqxpyzy4qme", "mm.example.com/hooks/…4qme"},
		{"discord-like id and token", "https://discord.example/api/webhooks/1234567890123456789/aBcDeFgHiJkLmNoPqRsTuVwXyZ012345", "discord.example/api/webhooks/1234567890123456789/…2345"},
		{"zapier-like key with trailing slash", "https://hooks.zapier.example/hooks/catch/123456/o2eyvv/", "hooks.zapier.example/hooks/catch/123456/…yvv/"},
		{"zapier-like key no trailing slash", "https://hooks.zapier.example/hooks/catch/123456/o2eyvv", "hooks.zapier.example/hooks/catch/123456/…yvv"},
		{"last mixed 5 shown", "https://h.example.com/hook/ab1cd", "h.example.com/hook/ab1cd"},
		{"last mixed 6 masked", "https://h.example.com/hook/ab1cde", "h.example.com/hook/…cde"},
		{"last mixed 7 masked", "https://h.example.com/hook/ab1cdef", "h.example.com/hook/…def"},
		{"digit-only tail masked", "https://h.example.com/hook/1234567", "h.example.com/hook/…567"},
		{"readable path shown whole", "https://hooks.acme.example/hooks/incidents", "hooks.acme.example/hooks/incidents"},
		{"letters-only word shown", "https://hooks.example.com/xsecrettail", "hooks.example.com/xsecrettail"},
		{"secret in query", "https://h.example.com/p?token=secretxyz123", "h.example.com/p?…"},
		{"secret in fragment", "https://h.example.com/p#fragsecret99", "h.example.com/p?…"},
		{"query on readable path", "https://example.com/hook?key=abc", "example.com/hook?…"},
		{"short path shown as is", "https://example.com/hook", "example.com/hook"},
		{"host only", "https://example.com", "example.com"},
		{"trailing slash", "https://example.com/", "example.com/"},
		{"host with port", "https://example.com:8443/hooksecret123", "example.com:8443/…t123"},
		{"userinfo hidden", "https://user:pass@hooks.example.com/a4d718d555cb88b0", "hooks.example.com/…88b0"},
		{"not a url", "just-a-plain-string", "…string"},
		{"short non-url shown as is", "abc", "abc"},
		{"unicode host and long path", "https://пример.рф/оченьдлинныйсекретныйпуть", "пример.рф/…путь"},
		{"long path collapses in the middle", "https://hooks.example.com/alpha/beta/gamma/delta/epsilon/zeta/theta/omega", "hooks.example.com/alpha/…/omega"},
		// maskChannelTarget (web/mask.go) уже отдаёт неадмину «scheme://host/… ·hhhh» —
		// повторная маска обязана сохранить дискриминатор ·hhhh, иначе вебхуки неразличимы.
		{"double mask keeps discriminator", "https://hooks.example.com/… ·ab12", "hooks.example.com/… ·ab12"},
	}
	for _, c := range cases {
		if got := maskedWebhookTarget(c.target); got != c.want {
			t.Errorf("%s: maskedWebhookTarget(%q) = %q, want %q", c.name, c.target, got, c.want)
		}
	}
}
