package auth_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

func TestVerifyPasswordMalformedVersionSegment(t *testing.T) {
	bad := "$argon2id$vX$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA"
	if _, err := auth.VerifyPassword("x", bad); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(битый v=) = %v, want ErrMalformedHash", err)
	}
}

func TestVerifyPasswordMalformedParamsSegment(t *testing.T) {
	bad := "$argon2id$v=19$mXt1p4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA"
	if _, err := auth.VerifyPassword("x", bad); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(битые параметры) = %v, want ErrMalformedHash", err)
	}
}

func TestVerifyPasswordRejectsTimeCostAboveCeiling(t *testing.T) {
	encoded, err := auth.HashPassword("boundary-check-time-cost")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("неожиданный формат PHC-строки от HashPassword: %v", parts)
	}
	// t=1 (argonTime) на выходе HashPassword всегда фиксирован.
	params := strings.Replace(parts[3], "t=1,", "t=17,", 1)
	if params == parts[3] {
		t.Fatalf("не удалось подменить t в сегменте параметров: %q", parts[3])
	}
	parts[3] = params
	mutated := strings.Join(parts, "$")

	if _, err := auth.VerifyPassword("boundary-check-time-cost", mutated); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(t=17) = %v, want ErrMalformedHash", err)
	}
}

func TestVerifyPasswordRejectsKeyShorterThanFloor(t *testing.T) {
	encoded, err := auth.HashPassword("boundary-check-key-length")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("неожиданный формат PHC-строки от HashPassword: %v", parts)
	}
	keyBytes, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	const shortLen = 12 // 8 <= 12 < 16: мутация "< 8" пропустит, "< 16" обязана поймать
	if len(keyBytes) <= shortLen {
		t.Fatalf("ключ от HashPassword короче ожидаемого (%d байт), тест некорректен", len(keyBytes))
	}
	parts[5] = base64.RawStdEncoding.EncodeToString(keyBytes[:shortLen])
	mutated := strings.Join(parts, "$")

	if _, err := auth.VerifyPassword("boundary-check-key-length", mutated); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(len(want)=%d) = %v, want ErrMalformedHash", shortLen, err)
	}
}
