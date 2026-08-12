package secretbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
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

// TestOpenBitFlippedCiphertext — qa P2-1: подмена одного байта в теле валидного
// ciphertext (после "enc:") должна ловиться Poly1305-тегом и давать ErrOpen, а
// не тихо возвращать мусор или паниковать. Механически это тот же код-путь, что
// и TestOpenWrongKey (secretbox.Open → ok=false → ErrOpen), но здесь неверен не
// ключ, а сами данные — это отдельный сценарий порчи (битый диск/ручное
// редактирование строки в БД), и стоит зафиксировать его отдельным тестом, а не
// полагаться на то, что оба пути читаются одинаково по коду.
func TestOpenBitFlippedCiphertext(t *testing.T) {
	key := sha256.Sum256([]byte("master"))
	sealed, err := Seal(key, "s3cr3t-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, EncPrefix))
	if err != nil {
		t.Fatalf("decode sealed payload: %v", err)
	}
	// Флип одного бита где-то в середине ciphertext (после 24-байтового nonce,
	// чтобы не просто менять nonce — тег обязан ловить порчу и там, и там, но
	// порча самого ciphertext ближе к реальному сценарию битого хранения).
	raw[len(raw)-1] ^= 0x01
	tampered := EncPrefix + base64.StdEncoding.EncodeToString(raw)

	if _, err := Open(key, tampered); err != ErrOpen {
		t.Fatalf("Open(tampered) = %v, want ErrOpen", err)
	}
}

// TestOpenLongRandomBase64TreatedAsCiphertext — qa P2-1, документирует
// пограничный случай из sealedPayload: длинное (>= minSealedLen), валидно
// декодируемое base64-значение, случайно начавшееся с "enc:", уходит в
// secretbox.Open и возвращает ErrOpen — а НЕ исходную строку как есть. Комментарий
// у sealedPayload описывает эту деградацию только для КОРОТКИХ/невалидных
// хвостов ("возвращается как есть"); для длинных валидных, но не настоящих
// sealed-значений поведение другое, и здесь оно зафиксировано явно, а не
// оставлено выводимым из чтения кода. Вероятность встретить такое в реальных
// данных мала (нужны 40+ байт валидного base64 не от Seal), но если встретится
// — сегодня это ErrOpen, а не значение как есть.
func TestOpenLongRandomBase64TreatedAsCiphertext(t *testing.T) {
	// minSealedLen = 24 (nonce) + secretbox.Overhead (16) = 40 байт. Берём с
	// запасом, чтобы не зависеть от точного значения константы.
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	legacyLookingValue := EncPrefix + base64.StdEncoding.EncodeToString(raw)

	key := sha256.Sum256([]byte("master"))
	got, err := Open(key, legacyLookingValue)
	if err != ErrOpen {
		t.Fatalf("Open(%q) = (%q,%v), want (\"\",ErrOpen) — current documented boundary behaviour, not the value as-is", legacyLookingValue, got, err)
	}
}

// TestIsEncrypted — распознаёт настоящий ciphertext (Seal) и отклоняет любой
// вид legacy plaintext, включая значение, случайно начавшееся с "enc:". Та же
// граница, что использует Open, чтобы решить, расшифровывать значение или
// вернуть его как есть; consumer'ы без мастер-ключа полагаются на IsEncrypted
// ровно в этой точке принятия решения.
func TestIsEncrypted(t *testing.T) {
	key := sha256.Sum256([]byte("master"))
	sealed, err := Seal(key, "s3cr3t-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsEncrypted(sealed) {
		t.Fatalf("IsEncrypted(%q) = false, want true for real ciphertext", sealed)
	}
	for _, in := range []string{"plainlegacy", "enc:not-base64!!", "enc:short", ""} {
		if IsEncrypted(in) {
			t.Fatalf("IsEncrypted(%q) = true, want false (legacy plaintext)", in)
		}
	}
}
