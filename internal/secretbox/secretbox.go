// Package secretbox — симметричное шифрование секретов at-rest (NaCl secretbox,
// XSalsa20-Poly1305) с маркером "enc:" и обратной совместимостью с legacy
// plaintext. Общий для org (SSO client_secret) и alert (секреты каналов),
// чтобы одинаково шифровать чувствительные значения в БД одним мастер-ключом.
//
// arch P2-2 (2026-08-12): sealed-формат ("enc:" + base64(nonce||ciphertext))
// НЕ несёт key-id/версию ключа — это осознанно, не упущение (см. обсуждение
// находки): добавление key-id было бы фичей (набор ключей, фоновая ротация),
// а не P2-фиксом. Следствие: смена GOTCHA_SECRET_KEY на работающем инстансе —
// ломающая операция, все существующие enc-значения становятся нерасшифруемыми
// разом (ErrOpen), путь миграции внутри формата отсутствует. Процедура ручной
// ротации задокументирована для операторов в internal/docs/{ru,en}/privacy.md
// («Смена GOTCHA_SECRET_KEY…» / «Rotating GOTCHA_SECRET_KEY…») и в таблице
// GOTCHA_SECRET_KEY в configuration.md.
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

// sealedPayload распознаёт stored как настоящий ciphertext, произведённый
// Seal: префикс "enc:" + валидный base64 длиной >= minSealedLen. Если нет —
// это legacy plaintext (в т.ч. случайно начавшийся с "enc:"), а не битый
// ciphertext. Общая проверка для Open и IsEncrypted, чтобы они не разъезжались.
func sealedPayload(stored string) ([]byte, bool) {
	if !strings.HasPrefix(stored, EncPrefix) {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, EncPrefix))
	if err != nil || len(raw) < minSealedLen {
		return nil, false
	}
	return raw, true
}

// IsEncrypted сообщает, является ли stored НАСТОЯЩИМ зашифрованным значением
// (произведённым Seal), а не legacy plaintext. Нужен вызывающим, у которых нет
// мастер-ключа (secretKeySet==false: dev-дефолт или откат GOTCHA_SECRET_KEY):
// они не могут вызвать Open, но обязаны отличить «значение и правда plaintext»
// от «значение зашифровано, но расшифровать нечем» — второе нельзя отдавать
// как живой секрет (сырой enc:-ciphertext вместо токена/пароля).
func IsEncrypted(stored string) bool {
	_, ok := sealedPayload(stored)
	return ok
}

// Open расшифровывает значение из Seal. Значение без префикса "enc:" считается
// legacy plaintext и возвращается как есть. Хвост после "enc:", не являющийся
// валидным base64 нужной длины, тоже трактуется как legacy plaintext (значение,
// случайно начавшееся с "enc:"), а не как битый ciphertext. Настоящий ciphertext
// с неверным ключом даёт ErrOpen.
func Open(key [32]byte, stored string) (string, error) {
	raw, ok := sealedPayload(stored)
	if !ok {
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
