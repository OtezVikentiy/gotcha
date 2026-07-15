package oauth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// cryptoSHA256 — алиас для rsa.VerifyPKCS1v15 (и тестового rsa.SignPKCS1v15).
const cryptoSHA256 = crypto.SHA256

var (
	ErrUnsupportedAlg = errors.New("oauth: unsupported id_token alg (want RS256)")
	ErrBadToken       = errors.New("oauth: malformed or unverifiable id_token")
)

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

// parseJWK строит rsa.PublicKey из JWK (n,e — base64url big-endian).
func parseJWK(k jwk) (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, ErrUnsupportedAlg
	}
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, ErrBadToken
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, ErrBadToken
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() > 1<<31 {
		return nil, ErrBadToken
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}

// verifyRS256 проверяет подпись id_token по JWKS и возвращает claims.
// iss/aud/exp/nonce НЕ проверяются здесь — это делает вызывающий (oidc.go).
func verifyRS256(token string, keys []jwk) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrBadToken
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrBadToken
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, ErrBadToken
	}
	if hdr.Alg != "RS256" {
		return nil, ErrUnsupportedAlg
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrBadToken
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))

	// Подбор ключа: по kid, иначе перебор всех RSA-ключей.
	candidates := keys
	if hdr.Kid != "" {
		candidates = nil
		for _, k := range keys {
			if k.Kid == hdr.Kid {
				candidates = append(candidates, k)
			}
		}
		if len(candidates) == 0 {
			candidates = keys // kid не нашёлся — пробуем все (ротация ключей)
		}
	}
	verified := false
	for _, k := range candidates {
		pub, err := parseJWK(k)
		if err != nil {
			continue
		}
		if rsa.VerifyPKCS1v15(pub, cryptoSHA256, sum[:], sig) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return nil, ErrBadToken
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrBadToken
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrBadToken, err)
	}
	return claims, nil
}
