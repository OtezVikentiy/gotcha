// Package secretbox — симметричное шифрование секретов at-rest (NaCl secretbox,
// XSalsa20-Poly1305) с маркером "enc:" и обратной совместимостью с legacy
// plaintext. Общий для org (SSO client_secret) и alert (секреты каналов),
// чтобы одинаково шифровать чувствительные значения в БД одним мастер-ключом.
package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// EncPrefix — маркер зашифрованного значения. Отсутствие префикса означает
// legacy plaintext (записи, сделанные до включения шифрования).
const EncPrefix = "enc:"

// minSealedLen — минимальная длина полезной нагрузки sealed-значения в байтах:
// nonce (24) + secretbox overhead (Poly1305-тег, 16) для пустого plaintext.
// Всё, что после "enc:" не декодится в валидный base64 длиной >= minSealedLen,
// считается legacy plaintext, случайно начавшимся с "enc:", а не битым
// ciphertext, — и возвращается как есть, без ошибки.
const minSealedLen = 24 + secretbox.Overhead

// ErrOpen — не удалось расшифровать enc-значение (битый ciphertext или неверный
// мастер-ключ).
var ErrOpen = errors.New("secretbox: cannot decrypt (wrong key or corrupt data)")

// Seal шифрует plaintext и возвращает "enc:" + base64(nonce24 || ciphertext).
// nonce — случайный из crypto/rand на каждый вызов.
func Seal(key [32]byte, plaintext string) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &key)
	return EncPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open расшифровывает значение из Seal. Значение без префикса "enc:" считается
// legacy plaintext и возвращается как есть. Хвост после "enc:", не являющийся
// валидным base64 нужной длины, тоже трактуется как legacy plaintext (значение,
// случайно начавшееся с "enc:"), а не как битый ciphertext. Настоящий ciphertext
// с неверным ключом даёт ErrOpen.
func Open(key [32]byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, EncPrefix) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, EncPrefix))
	if err != nil || len(raw) < minSealedLen {
		return stored, nil
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plaintext, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", ErrOpen
	}
	return string(plaintext), nil
}
