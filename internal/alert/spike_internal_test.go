package alert

import (
	"testing"
	"time"
)

func TestSpikeTickBudget(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"ниже пола — берём пол", time.Second, minSpikeTickBudget},
		{"выше пола — доля Interval", 60 * time.Second, 48 * time.Second},
		{"нулевой Interval — доля дефолта, как у Run", 0, time.Duration(float64(defaultSpikeInterval) * spikeTickBudgetShare)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &Spike{Interval: tt.interval}
			if got := sp.tickBudget(); got != tt.want {
				t.Errorf("tickBudget() при Interval=%v = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}
