package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
)

// TestOIDCExchangeIssuerMismatch (audit H9) — an id_token whose `iss` claim is
// neither the discovered issuer nor the configured issuer must be rejected.
// This is the OIDC token-confusion boundary: a token minted by a different IdP
// (or replayed from another tenant) must never produce a session.
func TestOIDCExchangeIssuerMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	// fakeOIDC merges these claims over its defaults, so `iss` here overrides
	// the honest srv.URL the discovery document advertises.
	srv := fakeOIDC(t, key, map[string]any{
		"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
		"iss": "https://evil.example/",
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})

	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if err == nil {
		t.Fatal("id_token with wrong iss must be rejected, got nil error")
	}
	if !errors.Is(err, ErrBadToken) {
		t.Fatalf("iss mismatch err = %v, want ErrBadToken", err)
	}
}

// TestOIDCExchangeAudienceMismatch (audit H9) — an id_token whose `aud` is a
// different client than the one configured must be rejected: a token minted
// for another relying party cannot be accepted here (account-takeover surface).
func TestOIDCExchangeAudienceMismatch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeOIDC(t, key, map[string]any{
		"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
		"aud": "some-other-client",
	})
	p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})

	_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
	if err == nil {
		t.Fatal("id_token with wrong aud must be rejected, got nil error")
	}
	if !errors.Is(err, ErrBadToken) {
		t.Fatalf("aud mismatch err = %v, want ErrBadToken", err)
	}
}

// TestOIDCExchangeAudienceArray (audit H9) — the `[]any` aud branch of
// audMatches (previously uncovered): an aud array is accepted only when it
// actually contains the configured client_id; otherwise rejected.
func TestOIDCExchangeAudienceArray(t *testing.T) {
	t.Run("array containing client_id is accepted", func(t *testing.T) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		srv := fakeOIDC(t, key, map[string]any{
			"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
			"aud": []any{"someone-else", "client-1"},
		})
		p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
		id, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
		if err != nil {
			t.Fatalf("aud array containing client_id must pass, got %v", err)
		}
		if id.Subject != "s" {
			t.Fatalf("Identity = %+v, want subject s", id)
		}
	})

	t.Run("array without client_id is rejected", func(t *testing.T) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		srv := fakeOIDC(t, key, map[string]any{
			"sub": "s", "email": "e@e.com", "email_verified": true, "nonce": "N1",
			"aud": []any{"someone-else", "another-client"},
		})
		p := NewOIDC(OIDCConfig{Issuer: srv.URL, ClientID: "client-1", ClientSecret: "secret"})
		_, err := p.Exchange(context.Background(), "c", "v", "https://gotcha/cb", "N1")
		if err == nil {
			t.Fatal("aud array without client_id must be rejected, got nil error")
		}
		if !errors.Is(err, ErrBadToken) {
			t.Fatalf("aud array mismatch err = %v, want ErrBadToken", err)
		}
	})
}
