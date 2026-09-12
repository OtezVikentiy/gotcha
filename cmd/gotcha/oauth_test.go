package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
)

func TestBuildRegistry(t *testing.T) {
	cfg := Config{
		OIDCEnabled: true, OIDCIssuer: "https://i", OIDCClientID: "c", OIDCClientSecret: "s",
		VKEnabled: true, VKClientID: "vc", VKClientSecret: "vs",
	}
	reg := buildRegistry(cfg)
	list := reg.List()
	if len(list) != 2 || list[0].Name() != "oidc" || list[1].Name() != "vk" {
		t.Fatalf("registry list = %v", list)
	}
	if !buildRegistry(Config{}).Empty() {
		t.Fatal("no providers → empty registry")
	}
}

// Мини-IdP только для проверки, что cfg.OIDCTrustEmail доезжает через
// buildRegistry до oauth.OIDCConfig.TrustEmail.
func fakeOIDCServer(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":         srv.URL,
			"token_endpoint": srv.URL + "/token",
			"jwks_uri":       srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kid": "k1", "kty": "RSA", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{
			"iss": srv.URL, "aud": "cid", "exp": float64(4102444800),
			"sub": "sub-1", "email": "wired@corp.com", "email_verified": true, "nonce": "N1",
		}
		idToken := signRS256Test(t, key, "k1", claims)
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "token_type": "Bearer"})
	})
	t.Cleanup(srv.Close)
	return srv
}

func signRS256Test(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestBuildRegistryWiresOIDCTrustEmail(t *testing.T) {
	oauth.SetAllowPrivateHosts(true)
	t.Cleanup(func() { oauth.SetAllowPrivateHosts(false) })

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv := fakeOIDCServer(t, key)

	for _, tc := range []struct {
		name       string
		trustEmail bool
	}{
		{"доверие отключено — Identity.TrustedIssuer=false", false},
		{"доверие включено — Identity.TrustedIssuer=true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				OIDCEnabled: true, OIDCIssuer: srv.URL, OIDCClientID: "cid", OIDCClientSecret: "csec",
				OIDCTrustEmail: tc.trustEmail,
			}
			reg := buildRegistry(cfg)
			p, ok := reg.Get("oidc")
			if !ok {
				t.Fatal("oidc provider not registered")
			}
			id, err := p.Exchange(context.Background(), "code", "verifier", "https://gotcha/cb", "N1")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if id.TrustedIssuer != tc.trustEmail {
				t.Fatalf("Identity.TrustedIssuer = %v, want %v (cfg.OIDCTrustEmail=%v)",
					id.TrustedIssuer, tc.trustEmail, tc.trustEmail)
			}
		})
	}
}
