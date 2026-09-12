package uptime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// должно совпадать с CHECK-ограничением monitors.kind.
type Kind string

const (
	KindHTTP      Kind = "http"
	KindTCP       Kind = "tcp"
	KindDNS       Kind = "dns"
	KindHeartbeat Kind = "heartbeat"
)

// нужен не коду, а сторожу динамических ключей i18n — читает значения из
// кода, а не литеральный список в тесте.
var Kinds = []string{string(KindHTTP), string(KindTCP), string(KindDNS), string(KindHeartbeat)}

// должно совпадать с CHECK-ограничением monitors.consensus.
type Consensus string

const (
	ConsensusAny      Consensus = "any"
	ConsensusMajority Consensus = "majority"
	ConsensusAll      Consensus = "all"
)

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

type TCPConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type DNSConfig struct {
	Hostname      string `json:"hostname"`
	RecordType    string `json:"record_type"` // A|AAAA|CNAME|MX|TXT
	ExpectedValue string `json:"expected_value,omitempty"`
}

type HeartbeatConfig struct {
	GraceSeconds int `json:"grace_seconds"` // >= 60
}

// шифрует ЗНАЧЕНИЯ заголовков (не имена) secretbox'ом с префиксом enc: — тот
// же приём, что alert.Service для секретов каналов.
func sealHTTPHeaders(ring secretbox.Keyring, raw json.RawMessage) (json.RawMessage, error) {
	return transformHTTPHeaders(raw, func(v string) (string, error) {
		// уже зашифрованное значение поднимаем Rewrap'ом до текущего ключа, не
		// шифруем заново Seal'ом — идемпотентно и не остаётся навсегда на старом ключе.
		if secretbox.IsEncrypted(v) {
			// ErrOpen (чужой/потерянный ключ) не пробрасываем — одно
			// нечитаемое значение не должно рушить сохранение всего конфига.
			out, _, _ := ring.Rewrap(v)
			return out, nil
		}
		return ring.Seal(v)
	})
}

// деградация по значению, не по строке — нечитаемый заголовок (запечатан
// потерянным ключом) не прерывает обработку соседних в той же строке.
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
	// changed=false возвращает raw БЕЗ ремаршалинга — RewrapSecrets сверяет
	// его байт-в-байт в CAS-предикате.
	if !changed {
		return raw, false, failures
	}
	cfg.Headers = next
	remarshaled, err := json.Marshal(cfg)
	if err != nil {
		// не должно падать (cfg только что разобран из валидного JSON); если
		// всё же случилось — не теряем raw, следующий рестарт попробует снова.
		return raw, false, failures
	}
	return remarshaled, true, failures
}

// legacy plaintext без enc: Keyring.Open возвращает как есть — совместимость
// со старыми записями.
func openHTTPHeaders(ring secretbox.Keyring, raw json.RawMessage) (json.RawMessage, error) {
	return transformHTTPHeaders(raw, func(v string) (string, error) {
		return ring.Open(v)
	})
}

// без этого при отсутствующем мастер-ключе сырой ciphertext уходил бы в
// исходящий HTTP-запрос чекера как значение заголовка; legacy plaintext не трогает.
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

// config непрозрачен (json.RawMessage) — сохраняются все поля, меняются
// только значения заголовков; декодируется нестрого, т.к. config уже прошёл validateConfig.
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

// ловит конфиг чужого kind — поля одного типа почти никогда не подмножество другого.
func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

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
	// HEAD без тела: BodyContains был бы всегда false (монитор вечно «упал»),
	// BodyNotContains всегда true (бессмысленно) — отклоняем на входе.
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
