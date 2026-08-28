package uptime

import (
	"encoding/json"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// TestSealHTTPHeadersEncryptsValueThatLooksLikeEncPrefix — значение заголовка,
// которое само по себе начинается с "enc:" (реальное, незашифрованное
// значение, не продукт Seal), обязано быть зашифровано, а не принято за уже
// зашифрованное и сохранено как plaintext.
//
// Раньше идемпотентность определялась голым strings.HasPrefix(v, "enc:") —
// такое значение прошло бы мимо Seal насквозь (P2-4 из аудита 2026-08-12).
func TestSealHTTPHeadersEncryptsValueThatLooksLikeEncPrefix(t *testing.T) {
	ring, err := secretbox.NewKeyring("seal-test-master-key-32-bytes!!", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	plaintextThatLooksSealed := "enc:not-actually-ciphertext"
	raw, err := json.Marshal(HTTPConfig{
		Method:  "GET",
		URL:     "https://example.com",
		Headers: map[string]string{"X-Token": plaintextThatLooksSealed},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	sealed, err := sealHTTPHeaders(ring, raw)
	if err != nil {
		t.Fatalf("sealHTTPHeaders: %v", err)
	}

	var cfg HTTPConfig
	if err := json.Unmarshal(sealed, &cfg); err != nil {
		t.Fatalf("unmarshal sealed config: %v", err)
	}
	stored := cfg.Headers["X-Token"]
	if stored == plaintextThatLooksSealed {
		t.Fatalf("value passed through unsealed: %q — should have been encrypted", stored)
	}
	if !secretbox.IsEncrypted(stored) {
		t.Fatalf("stored value %q is not real ciphertext", stored)
	}

	opened, err := ring.Open(stored)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != plaintextThatLooksSealed {
		t.Fatalf("Open() = %q, want %q", opened, plaintextThatLooksSealed)
	}
}

// TestSealHTTPHeadersIsIdempotentOnRealCiphertext — реальное enc:-значение
// (продукт Seal) не должно шифроваться повторно.
func TestSealHTTPHeadersIsIdempotentOnRealCiphertext(t *testing.T) {
	ring, err := secretbox.NewKeyring("seal-test-master-key-32-bytes!!", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	sealedOnce, err := ring.Seal("s3cr3t-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := json.Marshal(HTTPConfig{
		Method:  "GET",
		URL:     "https://example.com",
		Headers: map[string]string{"Authorization": sealedOnce},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	sealed, err := sealHTTPHeaders(ring, raw)
	if err != nil {
		t.Fatalf("sealHTTPHeaders: %v", err)
	}
	var cfg HTTPConfig
	if err := json.Unmarshal(sealed, &cfg); err != nil {
		t.Fatalf("unmarshal sealed config: %v", err)
	}
	if cfg.Headers["Authorization"] != sealedOnce {
		t.Fatalf("value was re-sealed: %q, want unchanged %q", cfg.Headers["Authorization"], sealedOnce)
	}
}
