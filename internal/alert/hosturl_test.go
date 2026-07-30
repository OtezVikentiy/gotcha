package alert

import "testing"

// TestHostOfURLRequiresHTTPScheme: хост извлекается только из абсолютного
// http(s)-адреса.
//
// url.Parse отдаёт хост и для «//evil.example/x», и для «ftp://…»; здесь
// решается, доверенный ли получатель деталей события, поэтому «хост непонятно
// какого протокола» доверенным быть не должен.
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

// TestDetailPolicyRejectsNonHTTPWebhook: канал с не-http(s) адресом не получает
// деталей события — даже если его «хост» совпал бы с доверенным.
func TestDetailPolicyRejectsNonHTTPWebhook(t *testing.T) {
	p := NewDetailPolicy("https://gotcha.example.com", []string{"acme.example"}, false)
	if p.AllowsDetails(Channel{Kind: ChannelWebhook, Target: "ftp://acme.example/hook"}) {
		t.Error("детали ушли в канал с ftp-адресом на доверенном домене")
	}
	if !p.AllowsDetails(Channel{Kind: ChannelWebhook, Target: "https://acme.example/hook"}) {
		t.Error("детали не ушли в обычный https-канал доверенного домена")
	}
}
