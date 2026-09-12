package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderNamesAndDisplay(t *testing.T) {
	oidc := NewOIDC(OIDCConfig{})
	if oidc.Name() != "oidc" || oidc.DisplayName() != "OIDC" {
		t.Fatalf("oidc name/display = %q/%q", oidc.Name(), oidc.DisplayName())
	}
	if custom := NewOIDC(OIDCConfig{DisplayName: "Corp SSO"}).DisplayName(); custom != "Corp SSO" {
		t.Fatalf("oidc custom DisplayName = %q", custom)
	}

	vk := NewVK(VKConfig{})
	if vk.Name() != "vk" || vk.DisplayName() != "VK" {
		t.Fatalf("vk name/display = %q/%q", vk.Name(), vk.DisplayName())
	}

	ya := NewYandex(YandexConfig{})
	if ya.Name() != "yandex" || ya.DisplayName() != "Yandex" {
		t.Fatalf("yandex name/display = %q/%q", ya.Name(), ya.DisplayName())
	}
}

func TestOIDCScopesCustomAndDefault(t *testing.T) {
	if got := NewOIDC(OIDCConfig{}).scopes(); got != "openid email profile" {
		t.Fatalf("default scopes = %q", got)
	}
	if got := NewOIDC(OIDCConfig{Scopes: "openid groups"}).scopes(); got != "openid groups" {
		t.Fatalf("custom scopes = %q", got)
	}
}

func TestOIDCScopesCommaSeparatedNormalizedToSpace(t *testing.T) {
	if got := NewOIDC(OIDCConfig{Scopes: "openid,email"}).scopes(); got != "openid email" {
		t.Fatalf("scopes() = %q, want %q", got, "openid email")
	}

	if got := NewOIDC(OIDCConfig{Scopes: " openid , ,email "}).scopes(); got != "openid email" {
		t.Fatalf("scopes() with blanks = %q, want %q", got, "openid email")
	}

	if got := NewOIDC(OIDCConfig{Scopes: " , , "}).scopes(); got != "openid email profile" {
		t.Fatalf("scopes() blank-only = %q, want default %q", got, "openid email profile")
	}
}

func TestRegistryNilReceiver(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("oidc"); ok {
		t.Fatal("nil registry Get должен вернуть !ok")
	}
	if r.List() != nil {
		t.Fatal("nil registry List должен вернуть nil")
	}
	if !r.Empty() {
		t.Fatal("nil registry должен быть Empty")
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("дубликат провайдера должен паниковать")
		}
	}()
	NewRegistry(stubProvider{"oidc"}, stubProvider{"oidc"})
}

func TestParseJWKErrors(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	goodN := base64.RawURLEncoding.EncodeToString(key.N.Bytes())

	t.Run("не-RSA kty", func(t *testing.T) {
		if _, err := parseJWK(jwk{Kty: "EC", N: goodN, E: "AQAB"}); !errors.Is(err, ErrUnsupportedAlg) {
			t.Fatalf("EC key err = %v, want ErrUnsupportedAlg", err)
		}
	})
	t.Run("битый base64 в N", func(t *testing.T) {
		if _, err := parseJWK(jwk{Kty: "RSA", N: "!!!не base64!!!", E: "AQAB"}); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad N err = %v, want ErrBadToken", err)
		}
	})
	t.Run("битый base64 в E", func(t *testing.T) {
		if _, err := parseJWK(jwk{Kty: "RSA", N: goodN, E: "!!!"}); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad E err = %v, want ErrBadToken", err)
		}
	})
	t.Run("слишком большая экспонента E", func(t *testing.T) {
		bigE := base64.RawURLEncoding.EncodeToString(big.NewInt(1 << 32).Bytes())
		if _, err := parseJWK(jwk{Kty: "RSA", N: goodN, E: bigE}); !errors.Is(err, ErrBadToken) {
			t.Fatalf("huge E err = %v, want ErrBadToken", err)
		}
	})
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signOverParts(t *testing.T, key *rsa.PrivateKey, headerB64, payloadRaw string) string {
	t.Helper()
	signing := headerB64 + "." + payloadRaw
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoSHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyRS256MalformedTokens(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys := []jwk{jwkFromKey(key, "k1")}
	rs256Hdr := b64json(map[string]any{"alg": "RS256", "kid": "k1"})

	t.Run("не три сегмента", func(t *testing.T) {
		if _, err := verifyRS256("a.b", keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("2 сегмента err = %v, want ErrBadToken", err)
		}
	})
	t.Run("битый base64 в заголовке", func(t *testing.T) {
		if _, err := verifyRS256("!!!.payload.sig", keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad header b64 err = %v, want ErrBadToken", err)
		}
	})
	t.Run("заголовок не JSON", func(t *testing.T) {
		hdr := base64.RawURLEncoding.EncodeToString([]byte("не json"))
		if _, err := verifyRS256(hdr+".payload.sig", keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad header json err = %v, want ErrBadToken", err)
		}
	})
	t.Run("битый base64 в подписи", func(t *testing.T) {
		tok := rs256Hdr + "." + b64json(map[string]any{"sub": "u"}) + ".!!!"
		if _, err := verifyRS256(tok, keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad sig b64 err = %v, want ErrBadToken", err)
		}
	})
	t.Run("тело не base64 (подпись валидна)", func(t *testing.T) {
		tok := signOverParts(t, key, rs256Hdr, "!!!не base64!!!")
		if _, err := verifyRS256(tok, keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad payload b64 err = %v, want ErrBadToken", err)
		}
	})
	t.Run("тело не JSON (подпись валидна)", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte("{битый json"))
		tok := signOverParts(t, key, rs256Hdr, payload)
		if _, err := verifyRS256(tok, keys); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad payload json err = %v, want ErrBadToken", err)
		}
	})
}

func TestVerifyRS256KidNotFoundFallsBackToAllKeys(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signRS256(t, key, "kX", map[string]any{"sub": "u1"})
	claims, err := verifyRS256(tok, []jwk{jwkFromKey(key, "k1")})
	if err != nil {
		t.Fatalf("kid-fallback verify: %v", err)
	}
	if claims["sub"] != "u1" {
		t.Fatalf("sub = %v", claims["sub"])
	}
}

func TestOIDCExchangeTokenEndpointError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := oidcServer(t, key, oidcHandlers{
		token: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if !errors.Is(err, ErrExchange) {
		t.Fatalf("token 500 err = %v, want ErrExchange", err)
	}
}

func TestOIDCExchangeNoIDToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := oidcServer(t, key, oidcHandlers{
		token: func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
		},
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if !errors.Is(err, ErrExchange) {
		t.Fatalf("no id_token err = %v, want ErrExchange", err)
	}
}

func TestOIDCExchangeNoSubject(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeOIDC(t, key, map[string]any{
		"email": "e@e.com", "email_verified": true, "nonce": "N1",
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if !errors.Is(err, ErrBadToken) {
		t.Fatalf("no sub err = %v, want ErrBadToken", err)
	}
}

func TestOIDCExchangeEmailFromUserinfo(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := oidcServer(t, key, oidcHandlers{
		token: func(w http.ResponseWriter, r *http.Request) {
			c := map[string]any{"aud": "client-1", "exp": float64(4102444800),
				"sub": "s", "nonce": "N1", "iss": serverBaseURL}
			idToken := signRS256(t, key, "k1", c)
			_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "access_token": "at"})
		},
		userinfo: func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer at" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "ui@corp.com", "email_verified": true})
		},
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	id, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if err != nil {
		t.Fatalf("Exchange with userinfo email: %v", err)
	}
	if id.Email != "ui@corp.com" || !id.EmailVerified {
		t.Fatalf("Identity = %+v, want email из userinfo", id)
	}
}

func TestOIDCAuthURLDiscoveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1"})
	if got := p.AuthURL("S", "N", "C", "https://gotcha/cb"); got != "" {
		t.Fatalf("AuthURL при недоступном discovery = %q, want пустую строку", got)
	}
}

func TestOIDCDiscoveryJWKSError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := oidcServer(t, key, oidcHandlers{
		jwks: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if !errors.Is(err, ErrExchange) {
		t.Fatalf("jwks 5xx err = %v, want ErrExchange", err)
	}
}

var serverBaseURL string

type oidcHandlers struct {
	token    http.HandlerFunc
	jwks     http.HandlerFunc
	userinfo http.HandlerFunc
}

func oidcServer(t *testing.T, key *rsa.PrivateKey, h oidcHandlers) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	serverBaseURL = srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
			"userinfo_endpoint":      srv.URL + "/userinfo",
		})
	})

	jwksH := h.jwks
	if jwksH == nil {
		jwksH = func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(jwks{Keys: []jwk{jwkFromKey(key, "k1")}})
		}
	}
	mux.HandleFunc("/jwks", jwksH)

	tokenH := h.token
	if tokenH == nil {
		tokenH = func(w http.ResponseWriter, r *http.Request) {
			c := map[string]any{"iss": srv.URL, "aud": "client-1", "exp": float64(4102444800),
				"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1"}
			_ = json.NewEncoder(w).Encode(map[string]any{"id_token": signRS256(t, key, "k1", c), "access_token": "at"})
		}
	}
	mux.HandleFunc("/token", tokenH)

	if h.userinfo != nil {
		mux.HandleFunc("/userinfo", h.userinfo)
	}

	t.Cleanup(srv.Close)
	return srv
}

func TestYandexExchangeTokenError(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	p := NewYandex(YandexConfig{ClientID: "c", ClientSecret: "s"})
	p.tokenURL = srv.URL + "/token"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); !errors.Is(err, ErrExchange) {
		t.Fatalf("yandex token 500 err = %v, want ErrExchange", err)
	}
}

func TestYandexExchangeNoAccessToken(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "bearer"})
	})
	p := NewYandex(YandexConfig{ClientID: "c", ClientSecret: "s"})
	p.tokenURL = srv.URL + "/token"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); !errors.Is(err, ErrExchange) {
		t.Fatalf("yandex no access_token err = %v, want ErrExchange", err)
	}
}

func TestYandexExchangeInfoStatusError(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	p := NewYandex(YandexConfig{ClientID: "c", ClientSecret: "s"})
	p.tokenURL = srv.URL + "/token"
	p.infoURL = srv.URL + "/info"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); !errors.Is(err, ErrExchange) {
		t.Fatalf("yandex info 503 err = %v, want ErrExchange", err)
	}
}

func TestVKExchangeTokenError(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	p := NewVK(VKConfig{ClientID: "c", ClientSecret: "s"})
	p.tokenURL = srv.URL + "/token"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); !errors.Is(err, ErrExchange) {
		t.Fatalf("vk token 500 err = %v, want ErrExchange", err)
	}
}

func TestVKExchangeBadTokenResponse(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
	})
	p := NewVK(VKConfig{ClientID: "c", ClientSecret: "s"})
	p.tokenURL = srv.URL + "/token"
	if _, err := p.Exchange(context.Background(), "c", "", "cb", ""); !errors.Is(err, ErrExchange) {
		t.Fatalf("vk bad token response err = %v, want ErrExchange", err)
	}
}

func TestGetJSONBadURL(t *testing.T) {
	if err := getJSON(context.Background(), "http://exa\x00mple", new(map[string]any)); err == nil {
		t.Fatal("getJSON с битым URL должен вернуть ошибку")
	}
}

func TestGetJSONConnRefused(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	if err := getJSON(context.Background(), url, new(map[string]any)); err == nil {
		t.Fatal("getJSON к закрытому серверу должен вернуть ошибку")
	}
}

func TestPostFormBadURL(t *testing.T) {
	if err := postForm(context.Background(), "http://exa\x00mple", nil, new(map[string]any)); err == nil {
		t.Fatal("postForm с битым URL должен вернуть ошибку")
	}
}
