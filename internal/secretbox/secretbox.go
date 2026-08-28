// Package secretbox — симметричное шифрование секретов at-rest (NaCl secretbox,
// XSalsa20-Poly1305) с маркером "enc:" и обратной совместимостью с legacy
// plaintext. Общий для org (SSO client_secret), alert (секреты каналов) и
// uptime (значения HTTP-заголовков монитора), чтобы одинаково шифровать
// чувствительные значения в БД одним мастер-ключом.
//
// Конверт версии 2 (см. keyring.go, Keyring.Seal) несёт id ключа, которым он
// запечатан: enc:v2:<key-id>:<base64(nonce24‖ciphertext)>. Раньше (arch P2-2,
// 2026-08-12) key-id отсутствовал осознанно — добавление сочли бы отдельной
// фичей (набор ключей, ротация), а не P2-фиксом; следствие — смена
// GOTCHA_SECRET_KEY на работающем инстансе была необратимо ломающей
// операцией, все enc-значения становились разом нерасшифруемыми. Теперь
// Keyring держит текущий и опциональный предыдущий ключ, id в конверте
// позволяет выбрать нужный на чтении, а Rewrap переводит значение на текущий
// ключ на лету — на записи и разовым бэкфиллом на старте. v1 (просто "enc:" +
// base64, без версии и id — формат этого пакета до задачи ротации) читается
// бессрочно: он уже в чужих БД. Пишется всегда только v2. Процедура ротации —
// internal/docs/{ru,en}/privacy.md, переменная GOTCHA_SECRET_KEY_PREV —
// таблица в configuration.md.
package secretbox

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// EncPrefix — маркер зашифрованного значения. Отсутствие префикса означает
// legacy plaintext (записи, сделанные до включения шифрования).
const EncPrefix = "enc:"

// v2Prefix — префикс конверта версии 2: за ним следуют 8 строчных hex-символов
// id ключа, двоеточие и base64(nonce24‖ciphertext). См. parseEnvelope.
const v2Prefix = "enc:v2:"

// minSealedLen — минимальная длина полезной нагрузки sealed-значения в байтах:
// nonce (24) + secretbox overhead (Poly1305-тег, 16) для пустого plaintext.
// Всё, что после префикса не декодится в валидный base64 такой длины,
// считается legacy plaintext, случайно начавшимся с "enc:", а не битым
// ciphertext.
const minSealedLen = 24 + secretbox.Overhead

// ErrOpen — не удалось расшифровать enc-значение: битый ciphertext, в кольце
// нет ключа с нужным id или конверт неизвестной версии. Единственная ошибка
// расшифровки — новые случаи её оборачивают (fmt.Errorf("%w: ...", ErrOpen,
// ...)), errors.Is по-прежнему узнаёт их все.
var ErrOpen = errors.New("secretbox: cannot decrypt (wrong key or corrupt data)")

// reVersionTag распознаёt версионированный конверт (enc:v<цифры>:) ДО того,
// как разбираться, валиден ли он как v2. Нужен, чтобы отличить «версия
// задана, но не открывается» (ErrOpen, fail closed) от «это просто plaintext,
// случайно начавшийся с enc:» (passthrough) — см. §3 спеки ротации.
var reVersionTag = regexp.MustCompile(`^enc:v[0-9]+:`)

// reV2Header — заголовок валидного конверта v2: enc:v2: + ровно 8 строчных
// hex-символов id + ":". Остаток строки — base64(nonce‖ciphertext).
var reV2Header = regexp.MustCompile(`^enc:v2:([0-9a-f]{8}):`)

// envVersion — класс значения, распознанный parseEnvelope.
type envVersion int

const (
	// envPlain — legacy plaintext, в т.ч. значения, случайно начавшиеся с
	// "enc:", но не являющиеся валидным конвертом никакой версии.
	envPlain envVersion = iota
	// envV1 — "enc:" + base64(nonce‖ciphertext) без тега версии (формат до
	// задачи ротации, читается бессрочно, но больше не пишется).
	envV1
	// envV2 — enc:v2:<id>:<base64>, валидный по форме конверт версии 2.
	envV2
	// envUnknown — распознан тег версии (enc:v<цифры>:), но конверт не
	// является валидным v2: другая версия, кривой hex в id, битый base64 или
	// слишком короткая нагрузка. Fail closed: это НЕ plaintext, Open обязан
	// отказать (ErrOpen), а не отдать значение как есть — иначе ciphertext
	// уходит наружу живым секретом.
	envUnknown
)

// envelope — результат разбора stored: класс плюс то, что нужно Keyring.Open
// (id ключа для v2, декодированные nonce‖ciphertext для v1/v2).
type envelope struct {
	version envVersion
	keyID   string
	raw     []byte
}

// parseEnvelope классифицирует stored по таблице §3 спеки ротации. Общая
// точка входа для IsEncrypted и Keyring.Open — чтобы они не могли разъехаться
// (пропущенный здесь класс тихо ломает оба сразу тем же способом).
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
		// Тег версии есть, но это не валидный v2 (другая версия, кривой hex
		// id, битый base64/короткая нагрузка) — неизвестная версия, не
		// plaintext.
		return envelope{version: envUnknown}
	}
	// Тега версии нет: единственная альтернатива plaintext — v1 (простой
	// "enc:" + base64, без версии).
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, EncPrefix))
	if err != nil || len(raw) < minSealedLen {
		return envelope{version: envPlain}
	}
	return envelope{version: envV1, raw: raw}
}

// IsEncrypted сообщает, является ли stored НАСТОЯЩИМ зашифрованным значением
// (v1, v2 или неизвестной версии), а не legacy plaintext. Нужен вызывающим,
// у которых нет ключа для расшифровки (secretKeySet==false: dev-дефолт или
// откат GOTCHA_SECRET_KEY): они не могут вызвать Open, но обязаны отличить
// «значение и правда plaintext» от «значение зашифровано, но расшифровать
// нечем» — второе нельзя отдавать как живой секрет. Критический инвариант: на
// нём стоят все такие ветки (scrubEncryptedHeaders, decryptSSO, ChannelSecret,
// идемпотентность sealHTTPHeaders). Неизвестная версия тоже считается
// зашифрованной: расшифровать её нечем, но это точно не живой секрет.
func IsEncrypted(stored string) bool {
	return parseEnvelope(stored).version != envPlain
}
