package secretbox

import (
	"crypto/sha256"
	"testing"
)

func TestSealOpenRoundtrip(t *testing.T) {
	key := sha256.Sum256([]byte("master"))
	sealed, err := Seal(key, "s3cr3t-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == "s3cr3t-token" || len(sealed) < len(EncPrefix)+1 || sealed[:len(EncPrefix)] != EncPrefix {
		t.Fatalf("sealed = %q, want enc-prefixed ciphertext", sealed)
	}
	got, err := Open(key, sealed)
	if err != nil || got != "s3cr3t-token" {
		t.Fatalf("Open = (%q,%v), want (s3cr3t-token,nil)", got, err)
	}
}

// TestOpenLegacyPlaintext — значение без префикса "enc:" (и «enc:», случайно
// начавшееся так, но не ciphertext) возвращается как есть, без ошибки.
func TestOpenLegacyPlaintext(t *testing.T) {
	key := sha256.Sum256([]byte("master"))
	for _, in := range []string{"plainlegacy", "enc:not-base64!!", "enc:short"} {
		got, err := Open(key, in)
		if err != nil || got != in {
			t.Fatalf("Open(%q) = (%q,%v), want (%q,nil)", in, got, err, in)
		}
	}
}

// TestOpenWrongKey — настоящий ciphertext с неверным ключом даёт ErrOpen.
func TestOpenWrongKey(t *testing.T) {
	sealed, _ := Seal(sha256.Sum256([]byte("k1")), "x")
	if _, err := Open(sha256.Sum256([]byte("k2")), sealed); err != ErrOpen {
		t.Fatalf("Open wrong key: err = %v, want ErrOpen", err)
	}
}
