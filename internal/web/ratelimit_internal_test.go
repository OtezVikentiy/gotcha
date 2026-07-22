package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

// TestClientIP закрепляет вывод реального IP клиента за/без доверенного прокси
// (SEC-L2). Ключевой инвариант безопасности: X-Forwarded-For доверяется ТОЛЬКО
// когда непосредственный пир входит в TrustedProxies — иначе клиент тривиально
// подделал бы заголовок и обошёл per-IP лимитер.
func TestClientIP(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8"), mustCIDR(t, "192.168.0.0/16")}
	cases := []struct {
		name               string
		trusted            []*net.IPNet
		remote, xff, want  string
	}{
		{"no trusted proxies -> RemoteAddr, XFF ignored", nil, "203.0.113.7:1234", "1.2.3.4", "203.0.113.7"},
		{"trusted peer -> client from XFF", trusted, "10.1.2.3:9", "203.0.113.9", "203.0.113.9"},
		{"trusted peer -> rightmost non-trusted in XFF chain", trusted, "10.1.2.3:9", "203.0.113.9, 10.9.9.9", "203.0.113.9"},
		{"untrusted peer -> XFF ignored (spoofing blocked)", trusted, "203.0.113.50:9", "1.2.3.4", "203.0.113.50"},
		{"trusted peer, empty XFF -> peer", trusted, "10.1.2.3:9", "", "10.1.2.3"},
		{"trusted peer, all XFF hops trusted -> peer", trusted, "10.1.2.3:9", "10.9.9.9, 192.168.1.1", "10.1.2.3"},
		{"trusted peer, garbage XFF token skipped", trusted, "10.1.2.3:9", "not-an-ip, 203.0.113.5", "203.0.113.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{TrustedProxies: c.trusted}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := h.clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRateLimitKey — ключ per-account лимитера: clientIP + '|' + нормализованный email.
func TestRateLimitKey(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	if got := h.rateLimitKey(r, "  User@Example.COM "); got != "203.0.113.7|user@example.com" {
		t.Errorf("rateLimitKey = %q, want %q", got, "203.0.113.7|user@example.com")
	}
}
