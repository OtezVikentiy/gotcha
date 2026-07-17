package org

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// TestSecretRoundtrip — sealSecret/openSecret восстанавливают исходный секрет,
// а зашифрованное значение несёт префикс "enc:" и отличается от plaintext.
func TestSecretRoundtrip(t *testing.T) {
	key := sha256.Sum256([]byte("master-key"))
	sealed, err := sealSecret(key, "s3cr3t")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("sealed %q has no enc: prefix", sealed)
	}
	if sealed == "s3cr3t" || strings.Contains(sealed, "s3cr3t") {
		t.Fatalf("sealed %q leaks plaintext", sealed)
	}
	got, err := openSecret(key, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("open = %q, want s3cr3t", got)
	}
}

// TestSecretLegacyPlaintext — openSecret на значении без префикса "enc:"
// возвращает его как есть (обратная совместимость со старыми записями).
func TestSecretLegacyPlaintext(t *testing.T) {
	key := sha256.Sum256([]byte("master-key"))
	got, err := openSecret(key, "plainlegacy")
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if got != "plainlegacy" {
		t.Fatalf("open legacy = %q, want plainlegacy", got)
	}
}

// TestSecretPlaintextLooksEncrypted — RA-L8: legacy plaintext, буквально
// начинающийся с "enc:" (записан в dev без мастер-ключа), НЕ должен приниматься
// за ciphertext. После включения ключа openSecret обязан вернуть такое значение
// как есть, а не падать ошибкой (иначе DoS SSO-конфига при переходе dev→prod).
func TestSecretPlaintextLooksEncrypted(t *testing.T) {
	key := sha256.Sum256([]byte("master-key"))
	// Значения, которые начинаются с "enc:", но не являются валидным
	// nonce||ciphertext: невалидный base64 и валидный, но слишком короткий.
	cases := []string{
		"enc:hello",         // "hello" — не кратно 4, не декодится в base64
		"enc:this is not b64@@", // явно не base64
		"enc:aGVsbG8=",      // валидный base64 "hello", но короче nonce+overhead
		"enc:",              // пустой хвост
	}
	for _, in := range cases {
		got, err := openSecret(key, in)
		if err != nil {
			t.Fatalf("openSecret(%q): неожиданная ошибка %v", in, err)
		}
		if got != in {
			t.Fatalf("openSecret(%q) = %q, want %q (вернуть plaintext как есть)", in, got, in)
		}
	}
}

// TestSecretWrongKey — расшифровка enc-значения чужим ключом → ошибка.
func TestSecretWrongKey(t *testing.T) {
	key := sha256.Sum256([]byte("master-key"))
	other := sha256.Sum256([]byte("another-key"))
	sealed, err := sealSecret(key, "s3cr3t")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := openSecret(other, sealed); err == nil {
		t.Fatalf("open with wrong key must fail")
	}
}
