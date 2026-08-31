package uptime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// Kind — тип монитора; совпадает с CHECK-ограничением monitors.kind.
type Kind string

const (
	KindHTTP      Kind = "http"
	KindTCP       Kind = "tcp"
	KindDNS       Kind = "dns"
	KindHeartbeat Kind = "heartbeat"
)

// Kinds — все типы монитора. Набор нужен не коду, а сторожу динамических
// ключей: он проверяет, что для каждого значения есть подпись в каталоге, и
// потому обязан читать значения из кода, а не из литерального списка в тесте.
var Kinds = []string{string(KindHTTP), string(KindTCP), string(KindDNS), string(KindHeartbeat)}

// Consensus — правило согласования результатов проверки по регионам;
// совпадает с CHECK-ограничением monitors.consensus.
type Consensus string

const (
	ConsensusAny      Consensus = "any"
	ConsensusMajority Consensus = "majority"
	ConsensusAll      Consensus = "all"
)

// HTTPConfig — конфиг монитора kind=http, сериализуется в monitors.config.
type HTTPConfig struct {
	Method          string            `json:"method"` // GET|POST|HEAD
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body,omitempty"`
	ExpectedStatus  []int             `json:"expected_status,omitempty"` // пусто = 200..299
	BodyContains    string            `json:"body_contains,omitempty"`
	BodyNotContains string            `json:"body_not_contains,omitempty"`
	FollowRedirects bool              `json:"follow_redirects"`
}

// TCPConfig — конфиг монитора kind=tcp.
type TCPConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// DNSConfig — конфиг монитора kind=dns.
type DNSConfig struct {
	Hostname      string `json:"hostname"`
	RecordType    string `json:"record_type"` // A|AAAA|CNAME|MX|TXT
	ExpectedValue string `json:"expected_value,omitempty"`
}

// HeartbeatConfig — конфиг монитора kind=heartbeat.
type HeartbeatConfig struct {
	GraceSeconds int `json:"grace_seconds"` // >= 60
}

// sealHTTPHeaders шифрует ЗНАЧЕНИЯ заголовков http-конфига secretbox'ом с
// префиксом enc: — имена остаются видимыми (секрет именно в значении: bearer-
// токен в Authorization, ключ в X-Api-Key). Тот же приём, что alert.Service для
// секретов каналов. Пустые заголовки и невалидный (не наш) config возвращаются
// без изменений; валидность самого config проверяет validateConfig отдельно.
func sealHTTPHeaders(ring secretbox.Keyring, raw json.RawMessage) (json.RawMessage, error) {
	return transformHTTPHeaders(raw, func(v string) (string, error) {
		// Уже зашифрованное значение не шифруем заново голым Seal — вместо
		// этого зовём Rewrap: конфиг, пришедший в запись нерасшифрованным
		// (bulk-edit, импорт, фид из List/GetBatch), поднимается до текущего
		// ключа кольца, а не остаётся навсегда на предыдущем. Идемпотентность
		// сохраняется — значение уже на текущем ключе Rewrap не трогает.
		// Нерасшифруемое значение Rewrap оставляет как есть: потерять его
		// хуже, чем сохранить нечитаемым.
		if secretbox.IsEncrypted(v) {
			// Rewrap возвращает нетронутое значение и ErrOpen, если ключа для
			// расшифровки в кольце нет (запись пришла с чужим/потерянным
			// ключом). Ошибку сюда не пробрасываем: одно нечитаемое значение
			// не должно рушить сохранение всего конфига монитора — оно
			// просто остаётся на прежнем ключе, как и раньше при passthrough.
			out, _, _ := ring.Rewrap(v)
			return out, nil
		}
		return ring.Seal(v)
	})
}

// rewrapHTTPHeaders поднимает ЧИТАЕМЫЕ значения заголовков http-конфига до
// конверта v2 текущего ключа кольца (Keyring.Rewrap) — рабочая лошадка
// RewrapSecrets (service.go). Деградация ПО ЗНАЧЕНИЮ, а не по строке: у
// монитора может быть один читаемый и один нечитаемый (запечатан потерянным
// ключом) заголовок разом. Нечитаемое значение остаётся как есть, его ошибка
// попадает в failures, но не прерывает обработку остальных заголовков этой
// же строки — иначе один потерянный ключ навсегда законсервировал бы
// соседний plaintext.
//
// changed=true, только если поднялось хотя бы одно значение — только тогда
// строку имеет смысл перезаписывать (и только тогда RewrapSecrets вообще
// шлёт UPDATE). При changed=false возвращается тот же raw БЕЗ ремаршалинга:
// не только последствий для заголовков нет, но и повода разойтись байт-в-
// байт со значением, которое RewrapSecrets использует в CAS-предикате.
func rewrapHTTPHeaders(ring secretbox.Keyring, raw json.RawMessage) (out json.RawMessage, changed bool, failures []error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, false, nil
	}
	var cfg HTTPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return raw, false, nil
	}
	if len(cfg.Headers) == 0 {
		return raw, false, nil
	}
	next := make(map[string]string, len(cfg.Headers))
	for name, val := range cfg.Headers {
		rewrapped, didChange, err := ring.Rewrap(val)
		if err != nil {
			failures = append(failures, fmt.Errorf("header %q: %w", name, err))
		}
		if didChange {
			changed = true
		}
		next[name] = rewrapped
	}
	if !changed {
		return raw, false, failures
	}
	cfg.Headers = next
	remarshaled, err := json.Marshal(cfg)
	if err != nil {
		// cfg только что разобран из валидного JSON — маршалинг обратно не
		// должен падать; если всё же случилось, не теряем raw, просто не
		// поднимаем строку в этом проходе (следующий рестарт попробует снова).
		return raw, false, failures
	}
	return remarshaled, true, failures
}

// openHTTPHeaders — обратная операция: расшифровывает значения заголовков.
// Legacy plaintext без префикса enc: Keyring.Open вернёт как есть
// (совместимость со старыми записями, сделанными до включения шифрования).
func openHTTPHeaders(ring secretbox.Keyring, raw json.RawMessage) (json.RawMessage, error) {
	return transformHTTPHeaders(raw, func(v string) (string, error) {
		return ring.Open(v)
	})
}

// scrubEncryptedHeaders обнуляет ЗНАЧЕНИЯ заголовков, которые являются
// настоящим enc:-ciphertext (secretbox.IsEncrypted) — вызывается, когда
// мастер-ключа нет (dev-дефолт или откат GOTCHA_SECRET_KEY) и расшифровать их
// нечем. Без этого openHTTPHeaders никогда бы не вызывался (no-op-ветка при
// !secretKeySet) и сырой ciphertext уходил бы дальше как значение заголовка —
// в исходящий HTTP-запрос чекера. Legacy plaintext (в т.ч. случайно
// начавшийся с "enc:") не трогает. scrubbed=true, если хотя бы одно значение
// обнулено — вызывающий логирует.
func scrubEncryptedHeaders(raw json.RawMessage) (out json.RawMessage, scrubbed bool, err error) {
	out, err = transformHTTPHeaders(raw, func(v string) (string, error) {
		if secretbox.IsEncrypted(v) {
			scrubbed = true
			return "", nil
		}
		return v, nil
	})
	return out, scrubbed, err
}

// transformHTTPHeaders применяет fn к каждому значению заголовков http-конфига и
// пересобирает config. Config — непрозрачный json.RawMessage, поэтому имена
// заголовков и прочие поля сохраняются, меняются только значения. Раскодируется
// нестрого (в отличие от validateConfig): здесь мы обрабатываем УЖЕ прошедший
// валидацию config, а не проверяем его.
func transformHTTPHeaders(raw json.RawMessage, fn func(string) (string, error)) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	var cfg HTTPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return raw, nil
	}
	if len(cfg.Headers) == 0 {
		return raw, nil
	}
	out := make(map[string]string, len(cfg.Headers))
	for name, val := range cfg.Headers {
		next, err := fn(val)
		if err != nil {
			return nil, err
		}
		out[name] = next
	}
	cfg.Headers = out
	remarshaled, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return remarshaled, nil
}

// strictUnmarshal декодирует raw в v, отклоняя незнакомые поля. Это ловит
// конфиг чужого типа (например, HTTPConfig для kind=tcp): поля одного
// типа конфига почти никогда не являются подмножеством другого.
func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// validateConfig проверяет, что raw — валидный конфиг для kind, и что он
// не содержит полей чужого типа конфига.
func validateConfig(kind Kind, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return invalid("", "config_required")
	}
	switch kind {
	case KindHTTP:
		var c HTTPConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return invalid("url", "config_http")
		}
		return validateHTTPConfig(c)
	case KindTCP:
		var c TCPConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return invalid("host", "config_tcp")
		}
		return validateTCPConfig(c)
	case KindDNS:
		var c DNSConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return invalid("hostname", "config_dns")
		}
		return validateDNSConfig(c)
	case KindHeartbeat:
		var c HeartbeatConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return invalid("grace_seconds", "config_heartbeat")
		}
		return validateHeartbeatConfig(c)
	default:
		return invalid("kind", "unknown_kind", "kind", string(kind))
	}
}

func validateHTTPConfig(c HTTPConfig) error {
	switch c.Method {
	case "GET", "POST", "HEAD":
	default:
		return invalid("method", "http_method")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return invalid("url", "http_url")
	}
	if len(c.Headers) > 20 {
		return invalid("headers", "http_headers_max", "max", "20")
	}
	for _, code := range c.ExpectedStatus {
		if code < 100 || code > 599 {
			return invalid("expected_status", "http_status_range")
		}
	}
	// HEAD-ответ по определению без тела: проверка BodyContains/
	// BodyNotContains у него всегда либо false (Contains — монитор вечно
	// «упал»), либо всегда true (NotContains — проверка бессмысленна).
	// Отклоняем на входе, а не даём монитору молча никогда не проходить.
	if c.Method == "HEAD" && (c.BodyContains != "" || c.BodyNotContains != "") {
		return invalid("body_contains", "http_head_body")
	}
	return nil
}

func validateTCPConfig(c TCPConfig) error {
	if c.Host == "" {
		return invalid("host", "tcp_host_required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return invalid("port", "tcp_port_range")
	}
	return nil
}

func validateDNSConfig(c DNSConfig) error {
	if c.Hostname == "" {
		return invalid("hostname", "dns_hostname_required")
	}
	switch c.RecordType {
	case "A", "AAAA", "CNAME", "MX", "TXT":
	default:
		return invalid("record_type", "dns_record_type")
	}
	return nil
}

func validateHeartbeatConfig(c HeartbeatConfig) error {
	if c.GraceSeconds < 60 {
		return invalid("grace_seconds", "heartbeat_grace_min", "min", "60")
	}
	return nil
}
