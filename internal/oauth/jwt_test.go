package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

// signRS256 — тестовый помощник: собирает JWT с RS256-подписью на заданном ключе.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoSHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwkFromKey(key *rsa.PrivateKey, kid string) jwk {
	return jwk{
		Kid: kid, Kty: "RSA", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func TestVerifyRS256Valid(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signRS256(t, key, "k1", map[string]any{"sub": "u1", "email": "u@e.com"})
	claims, err := verifyRS256(tok, []jwk{jwkFromKey(key, "k1")})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["sub"] != "u1" {
		t.Fatalf("sub = %v", claims["sub"])
	}
}

func TestVerifyRS256WrongKeyFails(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signRS256(t, key, "k1", map[string]any{"sub": "u1"})
	if _, err := verifyRS256(tok, []jwk{jwkFromKey(other, "k1")}); !errors.Is(err, ErrBadToken) {
		t.Fatalf("verify wrong key = %v, want ErrBadToken", err)
	}
}

func TestVerifyRS256RejectsNoneAlg(t *testing.T) {
	// alg=none → ErrUnsupportedAlg (защита от alg-downgrade).
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	tok := enc(map[string]any{"alg": "none"}) + "." + enc(map[string]any{"sub": "u1"}) + "."
	if _, err := verifyRS256(tok, nil); !errors.Is(err, ErrUnsupportedAlg) {
		t.Fatalf("verify none = %v, want ErrUnsupportedAlg", err)
	}
}
