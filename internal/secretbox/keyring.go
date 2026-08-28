package secretbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// keyIDDomain — метка домена для отпечатка ключа (key-id), отделяющая его от
// любого другого использования того же производного ключа. id — не секрет
// (32 бита односторонней функции, не даёт атакующему ничего сверх того, что
// уже даёт сам ciphertext), но домен исключает даже теоретическую путаницу с
// другим хешем от того же key.
const keyIDDomain = "gotcha-secretbox-keyid\x00"

// Keyring — набор ключей at-rest шифрования: текущим запечатывают (Seal),
// предыдущим (при наличии) только распечатывают. Так инстанс переживает
// ротацию GOTCHA_SECRET_KEY, не теряя то, что уже зашифровано старым ключом —
// Rewrap переводит такие значения на текущий ключ на лету.
type Keyring struct {
	cur     [32]byte
	curID   string
	prev    [32]byte
	prevID  string
	hasPrev bool
}

// deriveKey — sha256 от сырой строки мастер-ключа. Байт-в-байт та же
// деривация, что была в трёх сервисах (org/alert/uptime) до кольца: ею
// запечатаны все существующие v1-значения, менять нельзя — иначе они
// перестанут открываться.
func deriveKey(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}

// deriveKeyID — отпечаток производного ключа: hex(sha256(domain‖key))[:8].
func deriveKeyID(key [32]byte) string {
	h := sha256.New()
	h.Write([]byte(keyIDDomain))
	h.Write(key[:])
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

// NewKeyring строит кольцо из текущего и (опционально) предыдущего
// мастер-ключа. current не может быть пустым: кольцо без ключа для записи
// бессмысленно (dev-стенды с выключенным шифрованием кольцо не строят вовсе —
// см. вызывающих в cmd/gotcha/main.go). previous, равный current по
// ВЫВЕДЕННОМУ ключу (сырые строки могут отличаться, ключ — нет), тоже отказ:
// это не ротация, а конфигурационная ошибка, и молчаливо считать её нормой
// значило бы спрятать от оператора, что PREV ничего не делает.
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

// CurrentID — id текущего ключа кольца. Нужен для логов и диагностики:
// оператор вписывает его в проверочный SELECT при ротации (privacy.md).
func (r Keyring) CurrentID() string { return r.curID }

// PreviousID — id предыдущего ключа кольца, если он есть. Это и есть
// <old-id> из шага 2 процедуры ротации (privacy.md): оператор знает старый
// мастер-ключ (сам вписал его в GOTCHA_SECRET_KEY_PREV), но не его отпечаток —
// а без отпечатка проверочный SELECT, которым подтверждают, что в БД не
// осталось конвертов со старым ключом, составить нечем. Пустая строка
// означает «предыдущего ключа нет» и однозначно отличима от настоящего id:
// формат deriveKeyID всегда даёт 8 hex-символов.
func (r Keyring) PreviousID() string {
	if !r.hasPrev {
		return ""
	}
	return r.prevID
}

// Seal шифрует plaintext и возвращает конверт версии 2, запечатанный текущим
// ключом кольца. Пишется всегда только v2 — даже если в кольце есть
// предыдущий ключ (им только открывают, никогда не запечатывают).
func (r Keyring) Seal(plaintext string) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &r.cur)
	return fmt.Sprintf("%s%s:%s", v2Prefix, r.curID, base64.StdEncoding.EncodeToString(sealed)), nil
}

// keyByID возвращает ключ кольца с данным id (текущий или предыдущий).
func (r Keyring) keyByID(id string) ([32]byte, bool) {
	if id == r.curID {
		return r.cur, true
	}
	if r.hasPrev && id == r.prevID {
		return r.prev, true
	}
	return [32]byte{}, false
}

// openRaw открывает nonce24‖ciphertext ключом key.
func openRaw(raw []byte, key [32]byte) (string, bool) {
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plaintext, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", false
	}
	return string(plaintext), true
}

// Open расшифровывает значение, сохранённое Seal (v2) или старым форматом
// (v1), либо отдаёт legacy plaintext как есть.
//
// v2: ключ выбирается по id конверта, при отсутствии совпадения в кольце —
// сразу ErrOpen с id в сообщении (перебор бессмыслен: Poly1305 всё равно не
// сойдётся, а сообщение станет только хуже).
// v1: у него нет id, поэтому пробуем сначала текущий ключ, затем предыдущий.
// Конверт неизвестной версии — fail closed: ErrOpen, а не passthrough (см.
// envUnknown в secretbox.go).
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

// Rewrap приводит stored к конверту версии 2 текущего ключа, если это
// возможно.
//
// Пустая строка — «нет секрета», а не значение для шифрования: проходит без
// изменений. Это не мелочь — пустой Secret в UpdateChannel означает «оставить
// прежний» (internal/alert/alert.go), запечатанная пустая строка сломала бы
// оба смысла разом.
//
// Значение, уже лежащее в v2 текущего ключа, не трогается — важно и для
// идемпотентности CAS-бэкфилла (второй проход не должен считать «изменил» то,
// что не менялось), и для write-пути (sealHTTPHeaders).
//
// Нерасшифруемое значение НЕ трогается: возвращается как есть вместе с
// ErrOpen — потерять его хуже, чем оставить нечитаемым.
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
