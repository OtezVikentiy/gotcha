package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeOIDC поднимает минимальный OIDC-провайдер: discovery, jwks, token.
func fakeOIDC(t *testing.T, key *rsa.PrivateKey, claims map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
			"userinfo_endpoint":      srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks{Keys: []jwk{jwkFromKey(key, "k1")}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		c := map[string]any{"iss": srv.URL, "aud": "client-1", "exp": float64(4102444800)} // 2100 год
		for k, v := range claims {
			c[k] = v
		}
		idToken := signRS256(t, key, "k1", c)
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "access_token": "at", "token_type": "Bearer"})
	})
	t.Cleanup(srv.Close)
	return srv
}

func TestOIDCExchangeVerifiedEmail(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeOIDC(t, key, map[string]any{
		"sub": "oidc-sub-1", "email": "user@corp.com", "email_verified": true, "name": "User",
		"nonce": "N1",
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret", DisplayName: "Corp"})

	au := p.AuthURL("S1", "N1", "CHAL", "https://gotcha/cb")
	u, _ := url.Parse(au)
	if u.Query().Get("state") != "S1" || u.Query().Get("nonce") != "N1" ||
		u.Query().Get("code_challenge") != "CHAL" || u.Query().Get("client_id") != "client-1" {
		t.Fatalf("AuthURL query wrong: %s", au)
	}

	id, err := p.Exchange(context.Background(), "code-xyz", "VERIFIER", "https://gotcha/cb", "N1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "oidc-sub-1" || id.Email != "user@corp.com" || !id.EmailVerified {
		t.Fatalf("Identity = %+v", id)
	}
}

func TestOIDCExchangeExpiryLeeway(t *testing.T) {
	// SEC-L3: токен, истёкший в пределах clockSkewLeeway, принимается (дрейф часов IdP);
	// истёкший за пределами допуска — отклоняется.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)

	withinLeeway := fakeOIDC(t, key, map[string]any{
		"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
		"exp": float64(nowUnix() - 30), // истёк 30с назад, допуск 60с → ок
	})
	p := NewOIDC(OIDCConfig{Issuer: withinLeeway.URL, ClientID: "client-1", ClientSecret: "secret"})
	if _, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1"); err != nil {
		t.Fatalf("token within leeway must pass, got %v", err)
	}

	beyondLeeway := fakeOIDC(t, key, map[string]any{
		"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
		"exp": float64(nowUnix() - 120), // истёк 120с назад → за пределами допуска
	})
	p2 := NewOIDC(OIDCConfig{Issuer: beyondLeeway.URL, ClientID: "client-1", ClientSecret: "secret"})
	if _, err := p2.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1"); err == nil {
		t.Fatal("token expired beyond leeway must fail")
	}
}

func TestOIDCExchangeNotYetValid(t *testing.T) {
	// SEC-L3: nbf в будущем за пределами допуска → отклонение.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeOIDC(t, key, map[string]any{
		"sub": "s", "email": "e@e.com", "nonce": "N1",
		"nbf": float64(nowUnix() + 120),
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	if _, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1"); err == nil {
		t.Fatal("token with future nbf must fail")
	}
}

func TestOIDCExchangeNonceMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeOIDC(t, key, map[string]any{"sub": "s", "email": "e@e.com", "nonce": "OTHER"})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	if _, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1"); err == nil {
		t.Fatal("nonce mismatch must fail")
	}
}

func TestOIDCExchangeNoEmail(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	// Ни в claims, ни в userinfo (не зарегистрирован → 404) email нет.
	srv := fakeOIDC(t, key, map[string]any{"sub": "s", "nonce": "N1"})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	if _, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1"); err == nil {
		t.Fatal("missing email must fail")
	}
}
