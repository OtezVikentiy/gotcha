package web

import (
	"errors"
	"testing"
)

func TestFlowRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	f := oauthFlow{Provider: "oidc", State: "S", Nonce: "N", Verifier: "V", Link: true, UID: 42, IssuedAt: 1000}
	raw, err := signFlow(secret, f)
	if err != nil {
		t.Fatalf("signFlow: %v", err)
	}
	got, err := parseFlow(secret, raw, 1000+300) // в пределах TTL
	if err != nil {
		t.Fatalf("parseFlow: %v", err)
	}
	if got != f {
		t.Fatalf("round-trip = %+v, want %+v", got, f)
	}
}

func TestFlowTamperFails(t *testing.T) {
	secret := []byte("test-secret")
	raw, _ := signFlow(secret, oauthFlow{Provider: "oidc", State: "S", IssuedAt: 1000})
	if _, err := parseFlow([]byte("other-secret"), raw, 1000); !errors.Is(err, errFlowBadSig) {
		t.Fatalf("wrong secret = %v, want errFlowBadSig", err)
	}
	if _, err := parseFlow(secret, raw+"x", 1000); err == nil {
		t.Fatal("tampered payload must fail")
	}
}

func TestFlowExpires(t *testing.T) {
	secret := []byte("test-secret")
	raw, _ := signFlow(secret, oauthFlow{Provider: "oidc", IssuedAt: 1000})
	if _, err := parseFlow(secret, raw, 1000+oauthFlowTTL+1); !errors.Is(err, errFlowExpired) {
		t.Fatalf("expired = %v, want errFlowExpired", err)
	}
}
