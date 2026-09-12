package uptime_test

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestWindowIntervalsOneOffClipsToRange(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)

	start := from.Add(-time.Hour) // starts before the query range
	end := from.Add(2 * time.Hour)
	windows := []uptime.Window{{StartsAt: &start, EndsAt: &end, Timezone: "UTC"}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if !got[0].From.Equal(from) || !got[0].To.Equal(end) {
		t.Fatalf("got %+v, want From=%v To=%v", got[0], from, end)
	}
}

func TestWindowIntervalsOneOffOutsideRangeExcluded(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	start := to.Add(time.Hour)
	end := to.Add(2 * time.Hour)
	windows := []uptime.Window{{StartsAt: &start, EndsAt: &end, Timezone: "UTC"}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// «бессрочно» (EndsAt=nil) даёт интервал до конца запрошенного диапазона,
// не +∞ — иначе каждый вызывающий (uptime%, slo.excludeMaintenance) думал бы про бесконечность.
func TestWindowIntervalsOneOffUnboundedClampsToRequestedEnd(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)

	start := from.Add(-time.Hour) // starts before the query range, never ends
	windows := []uptime.Window{{StartsAt: &start, EndsAt: nil, Timezone: "UTC"}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if !got[0].From.Equal(from) || !got[0].To.Equal(to) {
		t.Fatalf("got %+v, want From=%v To=%v", got[0], from, to)
	}
}

func TestWindowIntervalsOneOffMissingFieldsSkipped(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	windows := []uptime.Window{{Timezone: "UTC"}} // StartsAt/EndsAt nil

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// 14-дневный диапазон с началом ровно на будний день окна захватывает два
// вхождения (день 0 и 7); третье, на границе диапазона, исключается (upper bound exclusive).
func TestWindowIntervalsWeeklyProducesOneIntervalPerOccurrence(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // Monday
	to := from.AddDate(0, 0, 14)
	windows := []uptime.Window{{
		Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "UTC",
	}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	want0From := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)
	want0To := time.Date(2024, 1, 1, 4, 0, 0, 0, time.UTC)
	if !got[0].From.Equal(want0From) || !got[0].To.Equal(want0To) {
		t.Fatalf("got[0] = %+v, want From=%v To=%v", got[0], want0From, want0To)
	}
	want1From := time.Date(2024, 1, 8, 2, 0, 0, 0, time.UTC)
	if !got[1].From.Equal(want1From) {
		t.Fatalf("got[1].From = %v, want %v", got[1].From, want1From)
	}
}

func TestWindowIntervalsWeeklyCrossesMidnight(t *testing.T) {
	// Monday 23:00 - Tuesday 01:00 UTC.
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	windows := []uptime.Window{{
		Weekly: true, Weekday: 1, StartTime: "23:00", EndTime: "01:00", Timezone: "UTC",
	}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	wantFrom := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC)
	wantTo := time.Date(2024, 1, 2, 1, 0, 0, 0, time.UTC)
	if !got[0].From.Equal(wantFrom) || !got[0].To.Equal(wantTo) {
		t.Fatalf("got[0] = %+v, want From=%v To=%v", got[0], wantFrom, wantTo)
	}
}

// возвращается в UTC-эквивалентных моментах независимо от таймзоны окна —
// Query.Uptime сравнивает с таймстампами ClickHouse (всегда UTC).
func TestWindowIntervalsWeeklyMoscowTimezoneConvertsToUTC(t *testing.T) {
	from := time.Date(2023, 12, 30, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	windows := []uptime.Window{{
		Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Moscow",
	}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	wantFrom := time.Date(2024, 1, 1, 2, 0, 0, 0, loc).UTC()
	wantTo := time.Date(2024, 1, 1, 4, 0, 0, 0, loc).UTC()
	if !got[0].From.Equal(wantFrom) || !got[0].To.Equal(wantTo) {
		t.Fatalf("got[0] = %+v, want From=%v To=%v", got[0], wantFrom, wantTo)
	}
}

func TestWindowIntervalsWeeklyInvalidTimezoneSkipped(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	windows := []uptime.Window{{
		Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Not/AZone",
	}}

	got := uptime.WindowIntervals(windows, from, to)
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty for invalid timezone", got)
	}
}

func TestWindowIntervalsEmptyInputs(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	if got := uptime.WindowIntervals(nil, from, to); len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}
