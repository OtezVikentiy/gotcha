package org

import (
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// sealSecret/openSecret — обёртки над общим internal/secretbox (см. его
// комментарии). Оставлены как тонкие функции, чтобы sso.go не менялся; логика
// шифрования и обратная совместимость с legacy plaintext — в общем пакете.
func sealSecret(key [32]byte, plaintext string) (string, error) {
	return secretbox.Seal(key, plaintext)
}

func openSecret(key [32]byte, stored string) (string, error) {
	return secretbox.Open(key, stored)
}
