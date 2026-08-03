package main

import "testing"

// TestHealthcheckURLFollowsAddr — дефолтный URL проверки следует за
// GOTCHA_ADDR: оператор, сменивший порт, не должен получать второй
// независимый симптом от той же правки (находка №99). Хост всегда 127.0.0.1:
// GOTCHA_ADDR — адрес прослушивания, адресом назначения он быть не может.
func TestHealthcheckURLFollowsAddr(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"", "http://127.0.0.1:8080/readyz"},
		{":9000", "http://127.0.0.1:9000/readyz"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000/readyz"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000/readyz"},
		{"[::]:9000", "http://127.0.0.1:9000/readyz"},
		{"мусор-без-порта", "http://127.0.0.1:8080/readyz"},
	}
	for _, c := range cases {
		getenv := func(k string) string {
			if k == "GOTCHA_ADDR" {
				return c.addr
			}
			return ""
		}
		url, ok := healthcheckRequested([]string{"--healthcheck"}, getenv)
		if !ok || url != c.want {
			t.Errorf("GOTCHA_ADDR=%q: url=%q ok=%v, want %q", c.addr, url, ok, c.want)
		}
	}
}

// TestHealthcheckExplicitURLWins — явный --healthcheck-url перекрывает
// и дефолт, и GOTCHA_ADDR.
func TestHealthcheckExplicitURLWins(t *testing.T) {
	getenv := func(string) string { return ":9000" }
	url, ok := healthcheckRequested(
		[]string{"--healthcheck", "--healthcheck-url=http://127.0.0.1:1234/readyz"}, getenv)
	if !ok || url != "http://127.0.0.1:1234/readyz" {
		t.Errorf("url=%q ok=%v, want явный URL", url, ok)
	}
}
