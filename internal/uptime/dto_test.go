package uptime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestJobDTORoundTrip: NewJobDTO вытаскивает из задания только то, что нужно
// чекеру (kind/config/timeout), а JobDTO.Monitor() восстанавливает из этого
// минимальный Monitor — id и поля совпадают в обе стороны.
func TestJobDTORoundTrip(t *testing.T) {
	cfg := json.RawMessage(`{"url":"https://x"}`)
	job := Job{
		QueueID:   7,
		MonitorID: 42,
		Region:    "eu",
		Monitor:   Monitor{ID: 42, Kind: KindHTTP, Config: cfg, TimeoutSeconds: 9},
	}
	dto := NewJobDTO(job)
	if dto.QueueID != 7 || dto.MonitorID != 42 || dto.Kind != KindHTTP || dto.TimeoutSeconds != 9 {
		t.Fatalf("NewJobDTO = %+v, want queue7/mon42/http/timeout9", dto)
	}
	if string(dto.Config) != string(cfg) {
		t.Errorf("dto.Config = %s, want %s", dto.Config, cfg)
	}
	m := dto.Monitor()
	if m.ID != 42 || m.Kind != KindHTTP || m.TimeoutSeconds != 9 || string(m.Config) != string(cfg) {
		t.Errorf("dto.Monitor() = %+v, want id42/http/timeout9", m)
	}
}

// TestResultDTORoundTrip: NewResultDTO раскладывает Result по сетевому DTO
// (тайминги в подобъект Timings), а ResultDTO.Result() собирает обратно —
// все поля переживают оба преобразования без потерь.
func TestResultDTORoundTrip(t *testing.T) {
	exp := time.Now().UTC().Add(48 * time.Hour)
	r := Result{
		OK: false, StatusCode: 503, Error: "boom",
		DNSMs: 1, ConnectMs: 2, TLSMs: 3, TTFBMs: 4, TotalMs: 10,
		BodySize: 128, SSLExpiresAt: &exp,
	}
	dto := NewResultDTO(99, r)
	if dto.QueueID != 99 || dto.OK != false || dto.StatusCode != 503 || dto.Error != "boom" {
		t.Fatalf("NewResultDTO head = %+v", dto)
	}
	if dto.Timings != (Timings{DNS: 1, Connect: 2, TLS: 3, TTFB: 4, Total: 10}) {
		t.Errorf("timings = %+v", dto.Timings)
	}
	back := dto.Result()
	if back.OK != r.OK || back.StatusCode != r.StatusCode || back.Error != r.Error ||
		back.DNSMs != r.DNSMs || back.ConnectMs != r.ConnectMs || back.TLSMs != r.TLSMs ||
		back.TTFBMs != r.TTFBMs || back.TotalMs != r.TotalMs || back.BodySize != r.BodySize {
		t.Errorf("Result() = %+v, want %+v", back, r)
	}
	if back.SSLExpiresAt == nil || !back.SSLExpiresAt.Equal(exp) {
		t.Errorf("SSLExpiresAt = %v, want %v", back.SSLExpiresAt, exp)
	}
}

func TestValidateTCPConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     TCPConfig
		wantErr bool
	}{
		{"empty host", TCPConfig{Host: "", Port: 80}, true},
		{"port zero", TCPConfig{Host: "h", Port: 0}, true},
		{"port too high", TCPConfig{Host: "h", Port: 70000}, true},
		{"valid", TCPConfig{Host: "h", Port: 443}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateTCPConfig(c.cfg)
			if c.wantErr && !errors.Is(err, ErrInvalidMonitor) {
				t.Errorf("err = %v, want ErrInvalidMonitor", err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func TestValidateHeartbeatConfig(t *testing.T) {
	if err := validateHeartbeatConfig(HeartbeatConfig{GraceSeconds: 30}); !errors.Is(err, ErrInvalidMonitor) {
		t.Errorf("grace 30: err = %v, want ErrInvalidMonitor", err)
	}
	if err := validateHeartbeatConfig(HeartbeatConfig{GraceSeconds: 60}); err != nil {
		t.Errorf("grace 60: err = %v, want nil", err)
	}
}
