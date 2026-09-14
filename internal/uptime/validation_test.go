package uptime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// причина отказа обязана доезжать машинным кодом, не только текстом —
// иначе веб-слою нечего переводить.
func TestValidationCarriesCodeAndField(t *testing.T) {
	cases := []struct {
		name      string
		monitor   Monitor
		regions   []string
		wantCode  string
		wantField string
	}{
		{
			name:      "битый адрес",
			monitor:   httpMonitor(`{"method":"GET","url":"not-a-url"}`),
			wantCode:  "http_url",
			wantField: "url",
		},
		{
			name:      "пустое имя",
			monitor:   named(httpMonitor(`{"method":"GET","url":"https://example.com"}`), ""),
			wantCode:  "name_length",
			wantField: "name",
		},
		{
			name: "таймаут больше интервала",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.TimeoutSeconds = 120
				m.IntervalSeconds = 60
				return m
			}(),
			wantCode:  "timeout_vs_interval",
			wantField: "timeout_seconds",
		},
		{
			name:      "слишком много регионов",
			monitor:   httpMonitor(`{"method":"GET","url":"https://example.com"}`),
			regions:   make([]string, maxRegions+1),
			wantCode:  "regions_max",
			wantField: "regions",
		},
		{
			// HEAD-ответ без тела: BodyContains у него всегда false — монитор вечно «упал» бы.
			name:      "HEAD с BodyContains",
			monitor:   httpMonitor(`{"method":"HEAD","url":"https://example.com","body_contains":"ok"}`),
			wantCode:  "http_head_body",
			wantField: "body_contains",
		},
		{
			name:      "HEAD с BodyNotContains",
			monitor:   httpMonitor(`{"method":"HEAD","url":"https://example.com","body_not_contains":"error"}`),
			wantCode:  "http_head_body",
			wantField: "body_contains",
		},
		{
			name: "неизвестный kind",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.Kind = "carrier-pigeon"
				return m
			}(),
			wantCode:  "unknown_kind",
			wantField: "kind",
		},
		{
			name: "интервал меньше минимума",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.IntervalSeconds = 29
				return m
			}(),
			wantCode:  "interval_min",
			wantField: "interval_seconds",
		},
		{
			name: "таймаут вне допустимого диапазона",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.TimeoutSeconds = 200
				return m
			}(),
			wantCode:  "timeout_range",
			wantField: "timeout_seconds",
		},
		{
			name: "повторов больше предела",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.Retries = 11
				return m
			}(),
			wantCode:  "retries_range",
			wantField: "retries",
		},
		{
			name: "нулевой порог падения",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.FailThreshold = 0
				return m
			}(),
			wantCode:  "fail_threshold_min",
			wantField: "fail_threshold",
		},
		{
			name: "нулевой порог восстановления",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.RecoveryThreshold = 0
				return m
			}(),
			wantCode:  "recovery_threshold_min",
			wantField: "recovery_threshold",
		},
		{
			name: "неизвестный consensus",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.Consensus = "quorum"
				return m
			}(),
			wantCode:  "consensus_invalid",
			wantField: "consensus",
		},
		{
			name: "отрицательный интервал напоминаний",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.RemindEveryMinutes = -1
				return m
			}(),
			wantCode:  "remind_min",
			wantField: "remind_every_minutes",
		},
		{
			name: "отрицательный SSLAlertDays",
			monitor: func() Monitor {
				m := httpMonitor(`{"method":"GET","url":"https://example.com"}`)
				m.SSLAlertDays = -1
				return m
			}(),
			wantCode:  "ssl_days_min",
			wantField: "ssl_alert_days",
		},
		{
			name:      "слишком длинное имя региона",
			monitor:   httpMonitor(`{"method":"GET","url":"https://example.com"}`),
			regions:   []string{strings.Repeat("x", maxRegionLen+1)},
			wantCode:  "region_length",
			wantField: "regions",
		},
		{
			name:      "пустой конфиг",
			monitor:   httpMonitor(""),
			wantCode:  "config_required",
			wantField: "",
		},
		{
			name:      "конфиг http с лишним полем",
			monitor:   httpMonitor(`{"method":"GET","url":"https://example.com","bogus":1}`),
			wantCode:  "config_http",
			wantField: "url",
		},
		{
			name:      "конфиг tcp с лишним полем",
			monitor:   tcpMonitor(`{"host":"db.internal","port":5432,"bogus":1}`),
			wantCode:  "config_tcp",
			wantField: "host",
		},
		{
			name:      "конфиг dns с лишним полем",
			monitor:   dnsMonitor(`{"hostname":"example.com","record_type":"A","bogus":1}`),
			wantCode:  "config_dns",
			wantField: "hostname",
		},
		{
			name:      "конфиг heartbeat с лишним полем",
			monitor:   heartbeatMonitor(`{"grace_seconds":60,"bogus":1}`),
			wantCode:  "config_heartbeat",
			wantField: "grace_seconds",
		},
		{
			name:      "недопустимый HTTP-метод",
			monitor:   httpMonitor(`{"method":"DELETE","url":"https://example.com"}`),
			wantCode:  "http_method",
			wantField: "method",
		},
		{
			name:      "ожидаемый статус вне диапазона",
			monitor:   httpMonitor(`{"method":"GET","url":"https://example.com","expected_status":[700]}`),
			wantCode:  "http_status_range",
			wantField: "expected_status",
		},
		{
			name:      "заголовков больше предела",
			monitor:   httpMonitor(manyHeadersHTTPConfig(21)),
			wantCode:  "http_headers_max",
			wantField: "headers",
		},
		{
			name:      "пустой tcp-хост",
			monitor:   tcpMonitor(`{"host":"","port":1234}`),
			wantCode:  "tcp_host_required",
			wantField: "host",
		},
		{
			name:      "tcp-порт вне диапазона",
			monitor:   tcpMonitor(`{"host":"db.internal","port":70000}`),
			wantCode:  "tcp_port_range",
			wantField: "port",
		},
		{
			name:      "пустой dns-hostname",
			monitor:   dnsMonitor(`{"hostname":"","record_type":"A"}`),
			wantCode:  "dns_hostname_required",
			wantField: "hostname",
		},
		{
			name:      "недопустимый dns record_type",
			monitor:   dnsMonitor(`{"hostname":"example.com","record_type":"PTR"}`),
			wantCode:  "dns_record_type",
			wantField: "record_type",
		},
		{
			name:      "heartbeat grace меньше минимума",
			monitor:   heartbeatMonitor(`{"grace_seconds":30}`),
			wantCode:  "heartbeat_grace_min",
			wantField: "grace_seconds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMonitor(tc.monitor, tc.regions)
			if err == nil {
				t.Fatal("ожидался отказ валидации")
			}
			if !errors.Is(err, ErrInvalidMonitor) {
				t.Errorf("errors.Is(ErrInvalidMonitor) = false — сломаются все вызывающие")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("отказ без кода (%v) — веб-слою нечего переводить", err)
			}
			if ve.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", ve.Code, tc.wantCode)
			}
			if ve.Field != tc.wantField {
				t.Errorf("Field = %q, want %q: без имени поля сообщение висит над формой, а не у виноватого поля",
					ve.Field, tc.wantField)
			}
		})
	}
}

func httpMonitor(cfg string) Monitor {
	return Monitor{
		Kind: KindHTTP, Name: "check", IntervalSeconds: 60, TimeoutSeconds: 10,
		FailThreshold: 1, RecoveryThreshold: 1, Consensus: ConsensusAny,
		Config: []byte(cfg),
	}
}

func named(m Monitor, name string) Monitor {
	m.Name = name
	return m
}

func tcpMonitor(cfg string) Monitor {
	return Monitor{
		Kind: KindTCP, Name: "check", IntervalSeconds: 60, TimeoutSeconds: 10,
		FailThreshold: 1, RecoveryThreshold: 1, Consensus: ConsensusAny,
		Config: []byte(cfg),
	}
}

func dnsMonitor(cfg string) Monitor {
	return Monitor{
		Kind: KindDNS, Name: "check", IntervalSeconds: 60, TimeoutSeconds: 10,
		FailThreshold: 1, RecoveryThreshold: 1, Consensus: ConsensusAny,
		Config: []byte(cfg),
	}
}

func heartbeatMonitor(cfg string) Monitor {
	return Monitor{
		Kind: KindHeartbeat, Name: "check", IntervalSeconds: 60, TimeoutSeconds: 10,
		FailThreshold: 1, RecoveryThreshold: 1, Consensus: ConsensusAny,
		Config: []byte(cfg),
	}
}

func manyHeadersHTTPConfig(n int) string {
	headers := make(map[string]string, n)
	for i := 0; i < n; i++ {
		headers[fmt.Sprintf("X-Header-%d", i)] = "v"
	}
	b, err := json.Marshal(HTTPConfig{Method: "GET", URL: "https://example.com", Headers: headers})
	if err != nil {
		panic(err)
	}
	return string(b)
}
