package uptime

import (
	"errors"
	"testing"
)

// TestValidationCarriesCodeAndField: причина отказа обязана доезжать до
// вызывающего машинным кодом, а не только текстом.
//
// Без кода веб-слою нечего переводить, и он показывал err.Error() как есть:
// «монитор: uptime: invalid monitor: http url must be a valid http(s) URL».
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
