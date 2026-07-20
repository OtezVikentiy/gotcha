package web

import (
	"strings"
	"testing"
	"time"
)

// TestFormatUSAxis — подпись оси несёт единицу измерения: пользователь не
// должен гадать, микросекунды это или миллисекунды. Ноль — исключение, «0µs»
// читалось бы как значащая величина.
func TestFormatUSAxis(t *testing.T) {
	cases := []struct {
		us   float64
		want string
	}{
		{0, "0"},
		{450, "450µs"},
		{50_000, "50ms"},
		{1_500_000, "1.5s"},
		{2_000_000, "2s"},
	}
	for _, c := range cases {
		if got := formatUSAxis(c.us); got != c.want {
			t.Errorf("formatUSAxis(%v) = %q, want %q", c.us, got, c.want)
		}
	}
}

// TestTimeAxisGranularity — шаг меток выбирается по длине окна: на неделе
// подписывать часы бессмысленно, на трёх часах — сутки.
func TestTimeAxisGranularity(t *testing.T) {
	mk := func(n int, step time.Duration) []time.Time {
		base := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
		out := make([]time.Time, n)
		for i := range out {
			out[i] = base.Add(time.Duration(i) * step)
		}
		return out
	}
	xFor := func(i int) float64 { return float64(i) * 100 }

	week := timeAxis(mk(14, 12*time.Hour), xFor, 10)
	if len(week) == 0 || !strings.Contains(week[0].text, ".") {
		t.Errorf("на недельном окне ожидались метки-даты, got %+v", week)
	}
	day := timeAxis(mk(24, time.Hour), xFor, 10)
	if len(day) == 0 || !strings.Contains(day[0].text, ":") {
		t.Errorf("на суточном окне ожидались метки-часы, got %+v", day)
	}
}

// TestTimeAxisRespectsMinGap — метки не ставятся чаще, чем раз в minGapPx:
// иначе подписи наезжают друг на друга.
func TestTimeAxisRespectsMinGap(t *testing.T) {
	base := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	times := make([]time.Time, 48)
	for i := range times {
		times[i] = base.Add(time.Duration(i) * time.Hour)
	}
	ticks := timeAxis(times, func(i int) float64 { return float64(i) * 5 }, 70)
	for i := 1; i < len(ticks); i++ {
		if gap := ticks[i].x - ticks[i-1].x; gap < 70 {
			t.Errorf("метки ближе минимального зазора: %v", gap)
		}
	}
}

// TestYScaleHeadroom — верх шкалы строго выше максимума, иначе самый высокий
// столбик упирается в рамку.
func TestYScaleHeadroom(t *testing.T) {
	if s := newYScale(10, 3); s.top <= 10 {
		t.Errorf("newYScale(10): top = %v, ожидался запас над максимумом", s.top)
	}
	if s := newYScaleFloat(90_000, 3); s.top <= 90_000 {
		t.Errorf("newYScaleFloat(90000): top = %v, ожидался запас над максимумом", s.top)
	}
}
