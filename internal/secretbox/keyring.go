package secretbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// id не секретен, но домен исключает даже теоретическую путаницу с другим
// хешем от того же производного ключа.
const keyIDDomain = "gotcha-secretbox-keyid\x00"

// текущим ключом Seal запечатывает, предыдущим (если есть) только распечатывает —
// так ротация GOTCHA_SECRET_KEY не теряет то, что уже зашифровано старым ключом.
type Keyring struct {
	cur     [32]byte
	curID   string
	prev    [32]byte
	prevID  string
	hasPrev bool
}

// та же деривация, что была в org/alert/uptime до кольца — менять нельзя,
// иначе все существующие v1-значения перестанут открываться.
func deriveKey(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}

// hex(sha256(domain‖key))[:8].
func deriveKeyID(key [32]byte) string {
	h := sha256.New()
	h.Write([]byte(keyIDDomain))
	h.Write(key[:])
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

// current обязателен; previous, совпадающий с current по выведенному ключу, —
// тоже отказ: молча принять его значило бы спрятать, что PREV ничего не делает.
func NewKeyring(current, previous string) (Keyring, error) {
	if current == "" {
		return Keyring{}, fmt.Errorf("secretbox: keyring requires a non-empty current key")
	}
	cur := deriveKey(current)
	r := Keyring{cur: cur, curID: deriveKeyID(cur)}
	if previous == "" {
		return r, nil
	}
	prev := deriveKey(previous)
	if prev == cur {
		return Keyring{}, fmt.Errorf("secretbox: keyring previous key must differ from current key")
	}
	r.prev = prev
	r.prevID = deriveKeyID(prev)
	r.hasPrev = true
	return r, nil
}

// нужен оператору для проверочного SELECT при ротации ключа (privacy.md).
func (r Keyring) CurrentID() string { return r.curID }

// пустая строка — «предыдущего нет», не спутать с id: deriveKeyID всегда
// даёт 8 hex-символов. Нужен оператору для проверочного SELECT при ротации.
func (r Keyring) PreviousID() string {
	if !r.hasPrev {
		return ""
	}
	return r.prevID
}

// всегда пишет v2 текущим ключом — предыдущим только открывают, никогда не запечатывают.
func (r Keyring) Seal(plaintext string) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &r.cur)
	return fmt.Sprintf("%s%s:%s", v2Prefix, r.curID, base64.StdEncoding.EncodeToString(sealed)), nil
}

func (r Keyring) keyByID(id string) ([32]byte, bool) {
	if id == r.curID {
		return r.cur, true
	}
	if r.hasPrev && id == r.prevID {
		return r.prev, true
	}
	return [32]byte{}, false
}

// первые 24 байта raw — nonce, остальное — ciphertext.
func openRaw(raw []byte, key [32]byte) (string, bool) {
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plaintext, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", false
	}
	return string(plaintext), true
}

// v2 выбирает ключ по id конверта, без перебора — Poly1305 всё равно не сойдётся
// с чужим ключом. Неизвестная версия — fail closed (ErrOpen), не passthrough.
func (r Keyring) Open(stored string) (string, error) {
	env := parseEnvelope(stored)
	switch env.version {
	case envPlain:
		return stored, nil
	case envV1:
		if pt, ok := openRaw(env.raw, r.cur); ok {
			return pt, nil
		}
		if r.hasPrev {
			if pt, ok := openRaw(env.raw, r.prev); ok {
				return pt, nil
			}
		}
		return "", ErrOpen
	case envV2:
		key, ok := r.keyByID(env.keyID)
		if !ok {
			return "", fmt.Errorf("%w: sealed with key id %s", ErrOpen, env.keyID)
		}
		if pt, ok := openRaw(env.raw, key); ok {
			return pt, nil
		}
		return "", ErrOpen
	default:
		return "", fmt.Errorf("%w: unknown envelope version", ErrOpen)
	}
}

// пустая строка — «нет секрета», не значение для шифрования, проходит без изменений.
// Нерасшифруемое — тоже без изменений, с ErrOpen: потерять хуже, чем оставить нечитаемым.
func (r Keyring) Rewrap(stored string) (string, bool, error) {
	if stored == "" {
		return stored, false, nil
	}
	env := parseEnvelope(stored)
	if env.version == envV2 && env.keyID == r.curID {
		return stored, false, nil
	}
	pt, err := r.Open(stored)
	if err != nil {
		return stored, false, err
	}
	sealed, err := r.Seal(pt)
	if err != nil {
		return stored, false, err
	}
	return sealed, true, nil
}
