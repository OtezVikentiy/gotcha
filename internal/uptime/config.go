package uptime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
)

// Kind — тип монитора; совпадает с CHECK-ограничением monitors.kind.
type Kind string

const (
	KindHTTP      Kind = "http"
	KindTCP       Kind = "tcp"
	KindDNS       Kind = "dns"
	KindHeartbeat Kind = "heartbeat"
)

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
		return fmt.Errorf("%w: config is required", ErrInvalidMonitor)
	}
	switch kind {
	case KindHTTP:
		var c HTTPConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: invalid http config: %v", ErrInvalidMonitor, err)
		}
		return validateHTTPConfig(c)
	case KindTCP:
		var c TCPConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: invalid tcp config: %v", ErrInvalidMonitor, err)
		}
		return validateTCPConfig(c)
	case KindDNS:
		var c DNSConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: invalid dns config: %v", ErrInvalidMonitor, err)
		}
		return validateDNSConfig(c)
	case KindHeartbeat:
		var c HeartbeatConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: invalid heartbeat config: %v", ErrInvalidMonitor, err)
		}
		return validateHeartbeatConfig(c)
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidMonitor, kind)
	}
}

func validateHTTPConfig(c HTTPConfig) error {
	switch c.Method {
	case "GET", "POST", "HEAD":
	default:
		return fmt.Errorf("%w: http method must be GET, POST or HEAD", ErrInvalidMonitor)
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: http url must be a valid http(s) URL", ErrInvalidMonitor)
	}
	if len(c.Headers) > 20 {
		return fmt.Errorf("%w: at most 20 http headers", ErrInvalidMonitor)
	}
	for _, code := range c.ExpectedStatus {
		if code < 100 || code > 599 {
			return fmt.Errorf("%w: expected_status codes must be 100..599", ErrInvalidMonitor)
		}
	}
	return nil
}

func validateTCPConfig(c TCPConfig) error {
	if c.Host == "" {
		return fmt.Errorf("%w: tcp host must not be empty", ErrInvalidMonitor)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: tcp port must be 1..65535", ErrInvalidMonitor)
	}
	return nil
}

func validateDNSConfig(c DNSConfig) error {
	if c.Hostname == "" {
		return fmt.Errorf("%w: dns hostname must not be empty", ErrInvalidMonitor)
	}
	switch c.RecordType {
	case "A", "AAAA", "CNAME", "MX", "TXT":
	default:
		return fmt.Errorf("%w: dns record_type must be A, AAAA, CNAME, MX or TXT", ErrInvalidMonitor)
	}
	return nil
}

func validateHeartbeatConfig(c HeartbeatConfig) error {
	if c.GraceSeconds < 60 {
		return fmt.Errorf("%w: heartbeat grace_seconds must be >= 60", ErrInvalidMonitor)
	}
	return nil
}
