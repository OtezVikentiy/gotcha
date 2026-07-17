package org

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// encPrefix — маркер зашифрованного значения. Отсутствие префикса означает
// legacy plaintext (записи, сделанные до включения шифрования).
const encPrefix = "enc:"

// minSealedLen — минимальная длина полезной нагрузки sealed-значения в байтах:
// nonce (24) + secretbox overhead (Poly1305-тег, 16) для пустого plaintext.
// Всё, что после "enc:" не декодится в валидный base64 длиной >= minSealedLen,
// считается legacy plaintext, случайно начавшимся с "enc:" (RA-L8), а не битым
// ciphertext, — и возвращается как есть, без ошибки.
const minSealedLen = 24 + secretbox.Overhead

// errOpenSecret — не удалось расшифровать enc-значение (битый ciphertext или
// неверный мастер-ключ).
var errOpenSecret = errors.New("org: cannot decrypt secret (wrong key or corrupt data)")

// sealSecret шифрует plaintext симметрично (NaCl secretbox, XSalsa20-Poly1305)
// и возвращает "enc:" + base64(nonce24 || ciphertext). nonce — случайный из
// crypto/rand на каждый вызов.
func sealSecret(key [32]byte, plaintext string) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	// secretbox.Seal дописывает ciphertext к переданному префиксу (nonce),
	// получая на выходе nonce||ciphertext.
	sealed := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &key)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// openSecret расшифровывает значение, полученное из sealSecret. Значение без
// префикса "enc:" считается legacy plaintext и возвращается как есть.
//
// RA-L8: одного префикса "enc:" мало — legacy plaintext (например записанный в
// dev до включения мастер-ключа client_secret вида "enc:...") может буквально
// начинаться с этих букв. Чтобы такое значение не приняли за ciphertext и не
// уронили чтение SSO-конфига, хвост после "enc:" дополнительно проверяется:
// это должен быть валидный base64 длиной не меньше minSealedLen. Если декод не
// удался или длина не сходится — трактуем как legacy plaintext и возвращаем как
// есть. Настоящий же ciphertext (валидный base64 нужной длины) с неверным
// ключом по-прежнему даёт ошибку, а не молчаливый «plaintext».
func openSecret(key [32]byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil || len(raw) < minSealedLen {
		// Не похоже на sealed-нагрузку → legacy plaintext, начавшийся с "enc:".
		return stored, nil
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plaintext, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", errOpenSecret
	}
	return string(plaintext), nil
}
