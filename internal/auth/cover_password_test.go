package auth_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
)

// TestVerifyPasswordMalformedVersionSegment — сегмент "v=%d" не парсится
// (нет знака "="), Sscanf возвращает ошибку раньше сравнения версий.
// Отличается от уже покрытого случая v=18 (валидный формат, но версия не
// совпадает) — здесь падает именно разбор самого сегмента.
func TestVerifyPasswordMalformedVersionSegment(t *testing.T) {
	bad := "$argon2id$vX$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA"
	if _, err := auth.VerifyPassword("x", bad); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(битый v=) = %v, want ErrMalformedHash", err)
	}
}

// TestVerifyPasswordMalformedParamsSegment — сегмент "m=%d,t=%d,p=%d" не
// парсится (нет "m="), Sscanf падает раньше проверки границ t/p/m.
// Отличается от уже покрытых случаев t=0/p=0/m=max (валидный формат сегмента,
// но значения вне допустимых границ) — здесь падает разбор самого сегмента.
func TestVerifyPasswordMalformedParamsSegment(t *testing.T) {
	bad := "$argon2id$v=19$mXt1p4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA"
	if _, err := auth.VerifyPassword("x", bad); !errors.Is(err, auth.ErrMalformedHash) {
		t.Fatalf("VerifyPassword(битые параметры) = %v, want ErrMalformedHash", err)
	}
}

// TestVerifyPasswordRejectsTimeCostAboveCeiling — t (аргон2 time cost) выше
// потолка 16 (см. комментарий в password.go про защиту от CPU-DoS: t приходит
// из PHC-строки в БД, гигантский t при большом m — неограниченный CPU).
// Берём НАСТОЯЩИЙ хеш от HashPassword (все сегменты валидны: версия, base64,
// длины) и точечно поднимаем t с 1 (реальное значение) до 17 — на единицу
// выше границы t<=16, а не на порядок, чтобы ловилась именно граница, а не
// что попало.
func TestVerifyPasswordRejectsTimeCostAboveCeiling(t *testing.T) {
	encoded, err := auth.HashPassword("boundary-check-time-cost")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("неожиданный формат PHC-строки от HashPassword: %v", parts)
	}
	// HashPassword всегда пишет t=1 (argonTime) — заменяем только его.
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

// TestVerifyPasswordRejectsKeyShorterThanFloor — len(want) строго между 8 и
// 16: не должна проходить мутацию границы `< 16` → `< 8` тоже незамеченной
// (12 не < 8, но обязано быть < 16). Остальные сегменты — из настоящего
// HashPassword-хеша (валидная соль, версия, параметры), только ключевой
// сегмент урезан по длине декодированных байт до 12.
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
