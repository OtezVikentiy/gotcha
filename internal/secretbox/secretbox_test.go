package secretbox

import (
	"errors"
	"testing"
)

// v1FixedVector — конверт версии 1 (простой "enc:" + base64(nonce‖ciphertext),
// без тега версии и id), запечатанный ДЕТЕРМИНИРОВАННЫМ nonce фиксированным
// ключом. Значение вписано константой, а не получено вызовом Seal/Keyring —
// это то, что реально лежит в чужих БД со времён до задачи ротации, и тест
// обязан проверять разбор именно такой байтовой строки, а не только то, что
// пакет умеет читать собственный вывод.
const (
	v1FixedMaster   = "vector-master-v1-legacy-old-code"
	v1FixedPlain    = "legacy-v1-secret-value"
	v1FixedEnvelope = "enc:AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYudf0xP3/sKnysGe0CDB7Uzw42DGYRgM/gl3FF8KMFQgpVnZw4I4="
)

// TestEnvelopeClassification — падающая таблица разбора конверта, §3 спеки
// ротации: шесть классов входа, для каждого проверяются и IsEncrypted (не
// требует ключей), и поведение Keyring.Open. Разошедшаяся классификация между
// этими двумя точками — ровно тот баг, которого таблица не пропускает мимо.
func TestEnvelopeClassification(t *testing.T) {
	ring, err := NewKeyring(v1FixedMaster, "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	v2Sealed, err := ring.Seal("v2-plaintext-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tests := []struct {
		name          string
		stored        string
		wantEncrypted bool
		// wantOpen задаётся для plaintext/v1/v2, успешно открывающихся этим
		// кольцом; wantErrOpen — для случаев, где Open обязан отказать.
		wantOpen    string
		wantErrOpen bool
	}{
		{
			name:          "без префикса enc: — legacy plaintext",
			stored:        "just-a-plain-value",
			wantEncrypted: false,
			wantOpen:      "just-a-plain-value",
		},
		{
			name:          "enc: + валидный base64 >= 40 байт — v1 ciphertext",
			stored:        v1FixedEnvelope,
			wantEncrypted: true,
			wantOpen:      v1FixedPlain,
		},
		{
			name:          "enc: + невалидный base64 — ложный enc:, plaintext",
			stored:        "enc:not-base64!!",
			wantEncrypted: false,
			wantOpen:      "enc:not-base64!!",
		},
		{
			name:          "enc: + валидный, но короткий base64 — ложный enc:, plaintext",
			stored:        "enc:aGk=", // decode("aGk=") = "hi", 2 байта < minSealedLen
			wantEncrypted: false,
			wantOpen:      "enc:aGk=",
		},
		{
			name:          "enc:v2: валидный конверт",
			stored:        v2Sealed,
			wantEncrypted: true,
			wantOpen:      "v2-plaintext-value",
		},
		{
			name:          "enc:v9: — неизвестная версия, НЕ plaintext",
			stored:        "enc:v9:" + v2Sealed[len(v2Prefix):],
			wantEncrypted: true,
			wantErrOpen:   true,
		},
		{
			name:          "enc:v2: с кривым hex id — неизвестная версия",
			stored:        "enc:v2:ZZZZZZZZ:" + v2Sealed[len(v2Prefix)+9:],
			wantEncrypted: true,
			wantErrOpen:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsEncrypted(tt.stored); got != tt.wantEncrypted {
				t.Fatalf("IsEncrypted(%q) = %v, want %v", tt.stored, got, tt.wantEncrypted)
			}
			got, err := ring.Open(tt.stored)
			if tt.wantErrOpen {
				if !errors.Is(err, ErrOpen) {
					t.Fatalf("Open(%q) err = %v, want ErrOpen", tt.stored, err)
				}
				return
			}
			if err != nil || got != tt.wantOpen {
				t.Fatalf("Open(%q) = (%q,%v), want (%q,nil)", tt.stored, got, err, tt.wantOpen)
			}
		})
	}
}

// TestIsEncryptedEmpty — пустая строка не считается зашифрованным значением
// (нет секрета вовсе, см. Rewrap("")).
func TestIsEncryptedEmpty(t *testing.T) {
	if IsEncrypted("") {
		t.Fatalf("IsEncrypted(\"\") = true, want false")
	}
}
