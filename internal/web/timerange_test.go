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
		wantWin := TimeRangePresets[key]
		if d := tr.Window(); absDur(d-wantWin) > time.Second {
			t.Errorf("period=%s window=%s want=%s", key, d, wantWin)
		}
		if tr.To.Before(now.Add(-time.Minute)) {
			t.Errorf("period=%s To=%v should be ~now", key, tr.To)
		}
	}
}

// TestParseTimeRangeAllHonoredOnlyWhenOffered (P1-6): период "all" даёт
// TimeRange{Key: RangeAll} (нулевые From/To — «без временного фильтра») ТОЛЬКО
// когда сама страница его предлагает, то есть def == RangeAll (issues.go —
// единственный вызывающий с AllowAll=true в селекторе). На любой другой
// странице (def — пресет, "24h" и т.п.) графику нужно окно всегда: autoStep
// делит на него, ось X строится по нему, а запрос с from==to==год-1 не
// находит в БД ни строки. Раньше period=all отдавал Key=RangeAll ВЕЗДЕ,
// невзирая на def, — страница без данных о нём молча превращалась в пустую.
func TestParseTimeRangeAllHonoredOnlyWhenOffered(t *testing.T) {
	// Страница, которая предлагает "all" (issues.go): период уважается,
	// границы пустые.
	tr := parseTimeRange(vals(map[string]string{"period": "all"}), RangeAll)
	if tr.Key != RangeAll || !tr.From.IsZero() || !tr.To.IsZero() {
		t.Errorf("def=RangeAll, period=all → %+v, want Key=all с нулевыми границами", tr)
	}

	// Страница, которая его НЕ предлагает (график с пресетом по умолчанию):
	// откат на дефолт страницы, как для любого нераспознанного period.
	for _, def := range []string{"24h", "7d", "30d"} {
		tr := parseTimeRange(vals(map[string]string{"period": "all"}), def)
		if tr.Key != def || tr.Custom {
			t.Errorf("def=%s, period=all → Key=%q Custom=%v, want откат на %s (all не предложен странице)",
				def, tr.Key, tr.Custom, def)
		}
		if tr.Window() <= 0 {
			t.Errorf("def=%s, period=all → окно %s, график не сможет построить ось", def, tr.Window())
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

func TestParseTimeRangeVisibleDatesWin(t *testing.T) {
	// Введённые видимые поля start+end включают произвольный диапазон САМИ,
	// даже если в списке стоит пресет — отдельно выбирать «свой диапазон» не
	// нужно (устранение лишнего действия).
	q := vals(map[string]string{
		"period": "7d",
		"start":  "2026-07-01T00:00",
		"end":    "2026-07-10T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if !tr.Custom {
		t.Errorf("filled start+end should activate custom over the preset: %+v", tr)
	}
}

func TestParseTimeRangeCarryOver(t *testing.T) {
	// period=custom + скрытые cstart/cend (видимые пусты) → перенесённый
	// активный произвольный диапазон сохраняется при смене прочих фильтров.
	q := vals(map[string]string{
		"period": "custom",
		"cstart": "2026-07-01T00:00",
		"cend":   "2026-07-10T00:00",
	})
	tr := parseTimeRange(q, "24h")
	if !tr.Custom {
		t.Errorf("carry-over cstart/cend should keep custom: %+v", tr)
	}

	// Выбор пресета в списке перебивает перенос custom (переключение обратно).
	q.Set("period", "7d")
	tr = parseTimeRange(q, "24h")
	if tr.Custom || tr.Key != "7d" {
		t.Errorf("preset should win over carried custom: %+v", tr)
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

// TestParseTimeRangeStartOnly (S4a): заполнено только «начало» — «с X и до сих
// пор» включает произвольный диапазон, конец = «сейчас». Раньше пустой end
// молча ронял ввод на пресет.
func TestParseTimeRangeStartOnly(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-48 * time.Hour).Format("2006-01-02T15:04")
	tr := parseTimeRange(vals(map[string]string{"start": start}), "24h")
	if !tr.Custom {
		t.Fatalf("start-only ввод должен включать custom, got %+v", tr)
	}
	if tr.To.After(now.Add(time.Minute)) {
		t.Errorf("конец должен быть ≈сейчас, got %v", tr.To)
	}
	if !tr.From.Before(tr.To) {
		t.Errorf("from должен быть раньше to: %+v", tr)
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
	// end присутствует, но не парсится → parseCustomRange подставляет «сейчас».
	now := time.Now().UTC()
	start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04")
	tr := parseTimeRange(vals(map[string]string{"start": start, "end": "garbage"}), "24h")
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
	cases := []struct {
		name       string
		window     time.Duration
		min, align time.Duration
		buckets    int
		want       time.Duration // точное ожидаемое значение
	}{
		{"perf 1h floors to min", time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 5 * time.Minute},
		{"perf 24h", 24 * time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 30 * time.Minute},
		{"perf 30d", 30 * 24 * time.Hour, 5 * time.Minute, 5 * time.Minute, 48, 15 * time.Hour},
		{"metrics 1h floors to min", time.Hour, time.Minute, 0, 120, time.Minute},
		{"buckets<1 normalizes to 1", time.Hour, time.Minute, 0, 0, time.Hour},
		// align round-up: 2h/7 = 17m8.57s → ближайшее кратное 5m вверх = 20m.
		{"align round-up branch", 2 * time.Hour, time.Minute, 5 * time.Minute, 7, 20 * time.Minute},
		// custom-окно, не делящееся на bucket-count: раньше давало нецелый шаг
		// (10801.07s) и расхождение с CH-сеткой — теперь ровно 10801s (B4).
		{"custom 7d+1m whole seconds", 7*24*time.Hour + time.Minute, 5 * time.Minute, 0, 56, 10801 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step := autoStep(c.window, c.min, c.align, c.buckets)
			if step != c.want {
				t.Errorf("autoStep(%s,%s,%s,%d) = %s, want %s", c.window, c.min, c.align, c.buckets, step, c.want)
			}
			// B4-инвариант: шаг всегда кратен целой секунде (совпадает с CH-сеткой).
			if step%time.Second != 0 {
				t.Errorf("autoStep(%s) = %s — не кратно секунде, CH-сетка разъедется", c.name, step)
			}
			// Назначение функции: шаг покрывает окно не более чем bucket-count слотами.
			b := c.buckets
			if b < 1 {
				b = 1
			}
			if step > 0 && c.window/step > time.Duration(b) {
				t.Errorf("autoStep(%s): window/step = %d > buckets %d", c.name, c.window/step, b)
			}
		})
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
