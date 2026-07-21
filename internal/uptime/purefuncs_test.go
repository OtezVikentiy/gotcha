package uptime

import (
	"math"
	"testing"
	"time"
)

// TestMsToUint32 — миллисекунды в uint32 с насыщением по краям.
func TestMsToUint32(t *testing.T) {
	if msToUint32(-time.Second) != 0 {
		t.Error("отрицательное → 0")
	}
	if msToUint32(150*time.Millisecond) != 150 {
		t.Error("150ms → 150")
	}
	if msToUint32(time.Duration(math.MaxUint32+1000) * time.Millisecond) != math.MaxUint32 {
		t.Error("переполнение → MaxUint32")
	}
}

// TestCauseFrom — причина инцидента: сначала ошибка своего состояния, затем
// первая ошибка среди down-регионов, иначе пусто.
func TestCauseFrom(t *testing.T) {
	if got := causeFrom(State{LastError: "timeout"}, nil); got != "timeout" {
		t.Errorf("своя ошибка: %q", got)
	}
	got := causeFrom(State{}, []State{{Status: "up"}, {Status: "down", LastError: "503"}})
	if got != "503" {
		t.Errorf("ошибка down-региона: %q", got)
	}
	if got := causeFrom(State{}, []State{{Status: "up"}}); got != "" {
		t.Errorf("нет ошибок → пусто, got %q", got)
	}
}

// TestValidateDNSConfig — пустой hostname и неизвестный тип записи отклоняются.
func TestValidateDNSConfig(t *testing.T) {
	if validateDNSConfig(DNSConfig{Hostname: "", RecordType: "A"}) == nil {
		t.Error("пустой hostname должен быть ошибкой")
	}
	if validateDNSConfig(DNSConfig{Hostname: "x.io", RecordType: "SRV"}) == nil {
		t.Error("SRV не поддерживается")
	}
	for _, rt := range []string{"A", "AAAA", "CNAME", "MX", "TXT"} {
		if err := validateDNSConfig(DNSConfig{Hostname: "x.io", RecordType: rt}); err != nil {
			t.Errorf("тип %s должен быть валиден: %v", rt, err)
		}
	}
}

// TestProbeClientDefaults — concurrency/pollEvery подставляют дефолты при <=0.
func TestProbeClientDefaults(t *testing.T) {
	c := &ProbeClient{}
	if c.concurrency() != defaultConcurrency {
		t.Errorf("дефолтный concurrency = %d", c.concurrency())
	}
	if c.pollEvery() != defaultPollEvery {
		t.Errorf("дефолтный pollEvery = %v", c.pollEvery())
	}
	c2 := &ProbeClient{Concurrency: 3, PollEvery: 2 * time.Second}
	if c2.concurrency() != 3 || c2.pollEvery() != 2*time.Second {
		t.Error("явные значения должны сохраняться")
	}
}
