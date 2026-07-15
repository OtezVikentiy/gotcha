package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestPKCEChallengeMatchesVerifier(t *testing.T) {
	verifier, challenge, err := PKCE()
	if err != nil {
		t.Fatalf("PKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatal("empty verifier/challenge")
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}
}

func TestRandomTokenUnique(t *testing.T) {
	a, err := RandomToken()
	if err != nil {
		t.Fatalf("RandomToken: %v", err)
	}
	b, _ := RandomToken()
	if a == "" || a == b {
		t.Fatalf("tokens not unique/nonempty: %q %q", a, b)
	}
}
