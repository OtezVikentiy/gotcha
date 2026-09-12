package alert

import "testing"

func TestHostOfURLRequiresHTTPScheme(t *testing.T) {
	cases := map[string]string{
		"https://hooks.acme.example/gotcha": "hooks.acme.example",
		"http://hooks.acme.example:8080/x":  "hooks.acme.example",
		"HTTPS://Hooks.Acme.Example/":       "hooks.acme.example",
		"//evil.example/x":                  "",
		"ftp://evil.example/x":              "",
		"javascript://evil.example/x":       "",
		"evil.example/x":                    "",
		"":                                  "",
	}
	for raw, want := range cases {
		if got := hostOfURL(raw); got != want {
			t.Errorf("hostOfURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDetailPolicyRejectsNonHTTPWebhook(t *testing.T) {
	p := NewDetailPolicy("https://gotcha.example.com", []string{"acme.example"}, false)
	if p.AllowsDetails(Channel{Kind: ChannelWebhook, Target: "ftp://acme.example/hook"}) {
		t.Error("детали ушли в канал с ftp-адресом на доверенном домене")
	}
	if !p.AllowsDetails(Channel{Kind: ChannelWebhook, Target: "https://acme.example/hook"}) {
		t.Error("детали не ушли в обычный https-канал доверенного домена")
	}
}
