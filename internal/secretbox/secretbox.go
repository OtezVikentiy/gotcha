package secretbox

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// отсутствие означает legacy plaintext — запись, сделанную до включения шифрования.
const EncPrefix = "enc:"

// за префиксом — 8 строчных hex-символов id ключа, двоеточие, base64(nonce24‖ciphertext).
const v2Prefix = "enc:v2:"

// nonce (24) + secretbox overhead (16) для пустого plaintext. Payload короче
// этого после префикса — legacy plaintext, случайно начавшийся с "enc:", не битый ciphertext.
const minSealedLen = 24 + secretbox.Overhead

// единственная ошибка расшифровки — новые случаи оборачивают её через
// fmt.Errorf("%w: ...", ErrOpen, ...), errors.Is узнаёт их все.
var ErrOpen = errors.New("secretbox: cannot decrypt (wrong key or corrupt data)")

// отличает «версия задана, но не открывается» (ErrOpen, fail closed) от
// «это просто plaintext, случайно начавшийся с enc:» (passthrough).
var reVersionTag = regexp.MustCompile(`^enc:v[0-9]+:`)

// enc:v2: + ровно 8 строчных hex-символов id + ":"; остаток — base64(nonce‖ciphertext).
var reV2Header = regexp.MustCompile(`^enc:v2:([0-9a-f]{8}):`)

type envVersion int

const (
	envPlain envVersion = iota
	envV1
	envV2
	// распознан тег версии, но конверт не валидный v2 (другая версия, кривой hex,
	// битый base64, короткая нагрузка) — fail closed: ErrOpen, не выдать ciphertext как есть.
	envUnknown
)

type envelope struct {
	version envVersion
	keyID   string
	raw     []byte
}

// общая точка входа для IsEncrypted и Keyring.Open — чтобы они не могли
// разъехаться (пропущенный здесь класс тихо ломает оба сразу).
func parseEnvelope(stored string) envelope {
	if !strings.HasPrefix(stored, EncPrefix) {
		return envelope{version: envPlain}
	}
	if reVersionTag.MatchString(stored) {
		if m := reV2Header.FindStringSubmatch(stored); m != nil {
			raw, err := base64.StdEncoding.DecodeString(stored[len(m[0]):])
			if err == nil && len(raw) >= minSealedLen {
				return envelope{version: envV2, keyID: m[1], raw: raw}
			}
		}
		return envelope{version: envUnknown}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, EncPrefix))
	if err != nil || len(raw) < minSealedLen {
		return envelope{version: envPlain}
	}
	return envelope{version: envV1, raw: raw}
}

// нужен вызывающим без ключа для расшифровки: они не могут вызвать Open, но обязаны
// отличить настоящий plaintext от «зашифровано, но расшифровать нечем» — второе не отдать как есть.
func IsEncrypted(stored string) bool {
	return parseEnvelope(stored).version != envPlain
}
