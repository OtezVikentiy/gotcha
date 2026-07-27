package web

import (
	"net/url"
	"testing"
	"time"
)

func vals(m map[string]string) url.Values {
	v := url.Values{}
	for k, val := range m {
		v.Set(k, val)
	}
	return v
}

func TestParseTimeRangePresets(t *testing.T) {
	now := time.Now().UTC()
	for _, key := range []string{"1h", "24h", "7d", "30d"} {
		tr := parseTimeRange(vals(map[string]string{"period": key}), "24h")
		if tr.Key != key || tr.Custom {
			t.Fatalf("period=%s → Key=%q Custom=%v", key, tr.Key, tr.Custom)
		}
		wantWin := timeRangePresets[key]
		if d := tr.Window(); absDur(d-wantWin) > time.Second {
			t.Errorf("period=%s window=%s want=%s", key, d, wantWin)
		}
		if tr.To.Before(now.Add(-time.Minute)) {
			t.Errorf("period=%s To=%v should be ~now", key, tr.To)
		}
	}
}

func TestParseTimeRangeDefaults(t *testing.T) {
	// пустой и неизвестный period → пресет по умолчанию.
	for _, p := range []string{"", "bogus", "99y"} {
		tr := parseTimeRange(vals(map[string]string{"period": p}), "7d")
		if tr.Key != "7d" || tr.Custom {
			t.Errorf("period=%q → Key=%q Custom=%v, want 7d preset", p, tr.Key, tr.Custom)
		}
	}
}

func TestParseTimeRangeCustom(t *testing.T) {
	q := vals(map[string]string{
		"period": "custom",
		"start":  "2026-07-01T00:00",
		"end":    "2026-07-10T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if !tr.Custom || tr.Key != "custom" {
		t.Fatalf("custom range not recognized: %+v", tr)
	}
	if tr.Window() != 9*24*time.Hour {
		t.Errorf("custom window = %s, want 216h", tr.Window())
	}
}

func TestParseTimeRangePresetWinsOverStaleInputs(t *testing.T) {
	// после переключения с custom на пресет поля start/end ещё держат
	// старые значения — известный пресет обязан их перебить.
	q := vals(map[string]string{
		"period": "7d",
		"start":  "2026-07-01T00:00",
		"end":    "2026-07-10T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if tr.Custom || tr.Key != "7d" {
		t.Errorf("preset should win over stale start/end: %+v", tr)
	}
}

func TestParseTimeRangeCustomFallsBackWhenInvalid(t *testing.T) {
	// start позже end — вырожденный диапазон, падаем на дефолт.
	q := vals(map[string]string{
		"start": "2026-07-10T00:00",
		"end":   "2026-07-01T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if tr.Custom || tr.Key != "24h" {
		t.Errorf("degenerate range should fall back to default: %+v", tr)
	}

	// нераспарсенный start — тоже дефолт.
	tr = parseTimeRange(vals(map[string]string{"start": "not-a-date"}), "24h")
	if tr.Custom || tr.Key != "24h" {
		t.Errorf("unparseable start should fall back: %+v", tr)
	}
}

func TestParseTimeRangeCustomClampsToRetention(t *testing.T) {
	q := vals(map[string]string{
		"start": "2020-01-01T00:00",
		"end":   "2026-01-01T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if !tr.Custom {
		t.Fatalf("expected custom, got %+v", tr)
	}
	if tr.Window() > timeRangeRetention {
		t.Errorf("window %s exceeds retention cap %s", tr.Window(), timeRangeRetention)
	}
}

func TestParseCustomRangeClampsFutureEnd(t *testing.T) {
	// end в будущем подтягивается к «сейчас» (ветка to.After(now)).
	now := time.Now().UTC()
	future := now.Add(48 * time.Hour).Format("2006-01-02T15:04")
	start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04")
	tr := parseTimeRange(vals(map[string]string{"start": start, "end": future}), "24h")
	if !tr.Custom {
		t.Fatalf("expected custom, got %+v", tr)
	}
	if tr.To.After(now.Add(time.Minute)) {
		t.Errorf("end not clamped to now: %v", tr.To)
	}
}

func TestParseTimeRangeCustomEndDefaultsToNow(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04")
	tr := parseTimeRange(vals(map[string]string{"start": start}), "24h")
	if !tr.Custom {
		t.Fatalf("expected custom, got %+v", tr)
	}
	if tr.To.Before(now.Add(-2 * time.Minute)) {
		t.Errorf("end should default to ~now, got %v", tr.To)
	}
}

func TestParseRangeTime(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"2026-07-01T15:04", true},
		{"2026-07-01T15:04:05", true},
		{"2026-07-01", true},
		{"2026-07-01T15:04:05Z", true},
		{"1751385600", true}, // unix seconds
		{"", false},
		{"garbage", false},
		{"-5", false},
	}
	for _, c := range cases {
		_, ok := parseRangeTime(c.in)
		if ok != c.ok {
			t.Errorf("parseRangeTime(%q) ok=%v, want %v", c.in, ok, c.ok)
		}
	}
}

func TestAutoStep(t *testing.T) {
	// perf: align=5m, min=5m — шаг всегда кратен 5 минутам и не мельче 5m.
	cases := []struct {
		window       time.Duration
		min, align   time.Duration
		buckets      int
		wantMultiple time.Duration
		wantMin      time.Duration
	}{
		{time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 5 * time.Minute, 5 * time.Minute},
		{24 * time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 5 * time.Minute, 5 * time.Minute},
		{30 * 24 * time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 5 * time.Minute, 5 * time.Minute},
		// метрики: align=0 — любой шаг ≥ min.
		{time.Hour, time.Minute, 0, 120, time.Nanosecond, time.Minute},
		// buckets<1 — защитная ветка (нормализуется к 1).
		{time.Hour, time.Minute, 0, 0, time.Nanosecond, time.Minute},
	}
	for _, c := range cases {
		step := autoStep(c.window, c.min, c.align, c.buckets)
		if step < c.wantMin {
			t.Errorf("autoStep(%s) = %s, below min %s", c.window, step, c.wantMin)
		}
		if c.align > 0 && step%c.align != 0 {
			t.Errorf("autoStep(%s) = %s, not multiple of %s", c.window, step, c.align)
		}
	}
}

func TestTimeRangeVM(t *testing.T) {
	// пресет: поля произвольного диапазона пусты (иначе форма ушла бы в custom).
	vm := timeRangeVM(TimeRange{Key: "24h"})
	if vm.Key != "24h" || vm.Custom || vm.Start != "" || vm.End != "" {
		t.Errorf("preset vm = %+v", vm)
	}
	// произвольный диапазон: границы отформатированы для datetime-local.
	tr := TimeRange{
		Key:    "custom",
		Custom: true,
		From:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
	}
	vm = timeRangeVM(tr)
	if !vm.Custom || vm.Start != "2026-07-01T00:00" || vm.End != "2026-07-10T00:00" {
		t.Errorf("custom vm = %+v", vm)
	}
}

func TestTimeRangeFieldValue(t *testing.T) {
	if got := timeRangeFieldValue(time.Time{}); got != "" {
		t.Errorf("zero time → %q, want empty", got)
	}
	ts := time.Date(2026, 7, 1, 15, 4, 0, 0, time.UTC)
	if got := timeRangeFieldValue(ts); got != "2026-07-01T15:04" {
		t.Errorf("field value = %q, want 2026-07-01T15:04", got)
	}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
