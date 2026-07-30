package uptime_test

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestWeeklyWindowKeepsDurationAcrossDST — окно длится заданное время и в ночь
// перевода часов.
//
// Измерено на Europe/Berlin для окна 02:00–04:00: весной time.Date нормализует
// несуществующие 02:00 в 03:00, а наивный конец 04:00 даёт окно длиной ОДИН час
// вместо двух. Осенью time.Date выбирает второе вхождение 02:00 (CET), и окно
// начинается на час позже намерения оператора. Оба случая приходятся ровно на
// ночь работ, когда окно и нужно.
//
// Окно теперь задаётся как «начало плюс длительность»: длительность реальная,
// потому что обслуживание описывает, сколько сервис фактически недоступен, и
// «два часа» должны означать два часа.
func TestWeeklyWindowKeepsDurationAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	w := uptime.Window{
		Weekly: true, Weekday: 0, // воскресенье — ночь перевода часов в ЕС
		StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Berlin",
	}

	cases := []struct {
		name string
		day  time.Time
	}{
		{"весенний перевод", time.Date(2026, 3, 29, 12, 0, 0, 0, berlin)},
		{"осенний перевод", time.Date(2026, 10, 25, 12, 0, 0, 0, berlin)},
		{"обычное воскресенье", time.Date(2026, 6, 7, 12, 0, 0, 0, berlin)},
	}
	for _, c := range cases {
		ivs := uptime.WindowIntervals([]uptime.Window{w},
			c.day.AddDate(0, 0, -1), c.day.AddDate(0, 0, 1))
		if len(ivs) != 1 {
			t.Errorf("%s: интервалов %d, want 1", c.name, len(ivs))
			continue
		}
		if got := ivs[0].To.Sub(ivs[0].From); got != 2*time.Hour {
			t.Errorf("%s: окно длится %v, want 2h (границы %s → %s)",
				c.name, got,
				ivs[0].From.In(berlin).Format("15:04 MST"),
				ivs[0].To.In(berlin).Format("15:04 MST"))
		}
	}
}

// TestAutumnWindowCoversFirstPass — осенью окно покрывает ПЕРВЫЙ проход
// удвоенного часа: работы идут тогда, когда их назначил оператор, а не часом
// позже.
func TestAutumnWindowCoversFirstPass(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	w := uptime.Window{
		Weekly: true, Weekday: 0,
		StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Berlin",
	}
	// 2026-10-25 00:30 UTC — это 02:30 CEST, первый проход, до перевода стрелок.
	firstPass := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	ivs := uptime.WindowIntervals([]uptime.Window{w},
		firstPass.Add(-3*time.Hour), firstPass.Add(6*time.Hour))
	if len(ivs) != 1 {
		t.Fatalf("интервалов %d, want 1", len(ivs))
	}
	if firstPass.Before(ivs[0].From) || firstPass.After(ivs[0].To) {
		t.Fatalf("первый проход 02:30 CEST (%s) вне окна %s → %s",
			firstPass.In(berlin).Format("15:04 MST"),
			ivs[0].From.In(berlin).Format("15:04 MST"),
			ivs[0].To.In(berlin).Format("15:04 MST"))
	}
}

// TestWindowUnaffectedInZoneWithoutDST — контроль: в поясе без перевода часов
// поведение не меняется. Иначе правка могла бы сдвинуть обычные окна, которых
// на инстансе большинство.
func TestWindowUnaffectedInZoneWithoutDST(t *testing.T) {
	ekb, err := time.LoadLocation("Asia/Yekaterinburg")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	w := uptime.Window{
		Weekly: true, Weekday: 2,
		StartTime: "23:00", EndTime: "01:00", // через полночь
		Timezone: "Asia/Yekaterinburg",
	}
	day := time.Date(2026, 7, 7, 12, 0, 0, 0, ekb) // вторник
	ivs := uptime.WindowIntervals([]uptime.Window{w},
		day.AddDate(0, 0, -1), day.AddDate(0, 0, 2))
	if len(ivs) != 1 {
		t.Fatalf("интервалов %d, want 1", len(ivs))
	}
	if got := ivs[0].To.Sub(ivs[0].From); got != 2*time.Hour {
		t.Fatalf("окно через полночь длится %v, want 2h", got)
	}
	if h := ivs[0].From.In(ekb).Hour(); h != 23 {
		t.Fatalf("окно начинается в %d:00, want 23:00", h)
	}
}
