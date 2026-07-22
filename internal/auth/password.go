// Package auth отвечает на вопрос «кто ты»: пароли, сессии, middleware.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Параметры argon2id — рекомендация RFC 9106 для интерактивного логина.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

var ErrMalformedHash = errors.New("auth: malformed password hash")

// HashPassword возвращает PHC-строку argon2id со случайной солью.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword сверяет пароль с PHC-строкой за константное время.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrMalformedHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrMalformedHash
	}
	if version != argon2.Version {
		return false, ErrMalformedHash
	}
	// Границы против паник и гигантских аллокаций/CPU в argon2.IDKey:
	// библиотека паникует при t<1 или p<1; m ограничиваем 2 GiB. t тоже
	// каппим сверху: стоимость argon2 линейна по t, а t приходит из PHC-строки
	// в БД — при её порче/подмене гигантский t (до 2^32) при m=2 GiB превратил
	// бы одну проверку пароля в неограниченный CPU-DoS. HashPassword всегда
	// пишет t=argonTime (маленькое), так что реальные хеши потолок не задевают.
	if t < 1 || t > 16 || p < 1 || m < 8*uint32(p) || m > 1<<21 {
		return false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrMalformedHash
	}
	// Пустые/короткие сегменты валидную PHC-строку не образуют: keyLen=0
	// роняет blake2b внутри argon2, а сравнение пустых ключей вырождается.
	if len(salt) == 0 || len(want) < 16 {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
