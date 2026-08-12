package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestCreateWindowOneOffActiveWithinAndOutsideInterval(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	start := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "Deploy",
		StartsAt:  &start,
		EndsAt:    &end,
		Timezone:  "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if w.ID == 0 {
		t.Fatalf("CreateWindow: id = 0")
	}

	active, err := svc.InMaintenance(ctx, pid, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("InMaintenance inside: %v", err)
	}
	if !active {
		t.Fatalf("InMaintenance inside window: got false, want true")
	}

	active, err = svc.InMaintenance(ctx, pid, end.Add(time.Hour))
	if err != nil {
		t.Fatalf("InMaintenance outside (after): %v", err)
	}
	if active {
		t.Fatalf("InMaintenance outside window (after): got true, want false")
	}

	active, err = svc.InMaintenance(ctx, pid, start.Add(-time.Hour))
	if err != nil {
		t.Fatalf("InMaintenance outside (before): %v", err)
	}
	if active {
		t.Fatalf("InMaintenance outside window (before): got true, want false")
	}

	windows, err := svc.Windows(ctx, pid)
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(windows) != 1 || windows[0].ID != w.ID || windows[0].Name != "Deploy" {
		t.Fatalf("Windows = %+v", windows)
	}

	if err := svc.DeleteWindow(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWindow: %v", err)
	}
	if err := svc.DeleteWindow(ctx, w.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("DeleteWindow again: err = %v, want ErrNotFound", err)
	}
}

func TestWeeklyWindowMoscowTimezone(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	// Monday 02:00-04:00 Europe/Moscow.
	if _, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "Weekly maintenance",
		Weekly:    true,
		Weekday:   1, // Monday (Go's time.Weekday: Sunday=0..Saturday=6)
		StartTime: "02:00",
		EndTime:   "04:00",
		Timezone:  "Europe/Moscow",
	}); err != nil {
		t.Fatalf("CreateWindow weekly: %v", err)
	}

	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// 2024-01-01 is a known Monday.
	inside := time.Date(2024, 1, 1, 3, 0, 0, 0, loc)
	if inside.Weekday() != time.Monday {
		t.Fatalf("test setup: %v is not Monday", inside)
	}
	active, err := svc.InMaintenance(ctx, pid, inside)
	if err != nil {
		t.Fatalf("InMaintenance weekly inside: %v", err)
	}
	if !active {
		t.Fatalf("InMaintenance weekly inside: got false, want true")
	}

	// Same instant expressed in a different timezone must still land
	// inside the window, since InMaintenance converts using the window's
	// own timezone.
	activeFromUTC, err := svc.InMaintenance(ctx, pid, inside.UTC())
	if err != nil {
		t.Fatalf("InMaintenance weekly inside (utc instant): %v", err)
	}
	if !activeFromUTC {
		t.Fatalf("InMaintenance weekly inside (utc instant): got false, want true")
	}

	outsideTime := time.Date(2024, 1, 1, 10, 0, 0, 0, loc)
	active, err = svc.InMaintenance(ctx, pid, outsideTime)
	if err != nil {
		t.Fatalf("InMaintenance weekly outside (time): %v", err)
	}
	if active {
		t.Fatalf("InMaintenance weekly outside (time): got true, want false")
	}

	// 2024-01-02 is Tuesday: same time-of-day, wrong weekday.
	wrongWeekday := time.Date(2024, 1, 2, 3, 0, 0, 0, loc)
	active, err = svc.InMaintenance(ctx, pid, wrongWeekday)
	if err != nil {
		t.Fatalf("InMaintenance weekly wrong weekday: %v", err)
	}
	if active {
		t.Fatalf("InMaintenance weekly wrong weekday: got true, want false")
	}
}

func TestWeeklyWindowCrossesMidnight(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	// Monday 23:00 - Tuesday 01:00 UTC.
	if _, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "Overnight",
		Weekly:    true,
		Weekday:   1, // Monday
		StartTime: "23:00",
		EndTime:   "01:00",
		Timezone:  "UTC",
	}); err != nil {
		t.Fatalf("CreateWindow crossing midnight: %v", err)
	}

	mondayLate := time.Date(2024, 1, 1, 23, 30, 0, 0, time.UTC)  // Monday 23:30
	tuesdayEarly := time.Date(2024, 1, 2, 0, 30, 0, 0, time.UTC) // Tuesday 00:30
	mondayEarly := time.Date(2024, 1, 1, 22, 0, 0, 0, time.UTC)  // Monday 22:00, before window

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"monday late", mondayLate, true},
		{"tuesday early (spillover)", tuesdayEarly, true},
		{"monday before window", mondayEarly, false},
	} {
		active, err := svc.InMaintenance(ctx, pid, tc.at)
		if err != nil {
			t.Fatalf("%s: InMaintenance: %v", tc.name, err)
		}
		if active != tc.want {
			t.Errorf("%s: InMaintenance(%v) = %v, want %v", tc.name, tc.at, active, tc.want)
		}
	}
}

func TestCreateWindowInvalidTimezone(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	_, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "Bad TZ",
		Weekly:    true,
		Weekday:   1,
		StartTime: "02:00",
		EndTime:   "04:00",
		Timezone:  "Not/AZone",
	})
	if !errors.Is(err, uptime.ErrInvalidWindow) {
		t.Fatalf("CreateWindow bad tz: err = %v, want ErrInvalidWindow", err)
	}
}

func TestCreateWindowInvalidFields(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	cases := map[string]uptime.Window{
		"bad weekday": {
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 7,
			StartTime: "02:00", EndTime: "04:00", Timezone: "UTC",
		},
		"bad start time": {
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "25:00", EndTime: "04:00", Timezone: "UTC",
		},
		"bad end time": {
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "not-a-time", Timezone: "UTC",
		},
		"one-off missing range": {
			ProjectID: pid, Name: "x", Weekly: false, Timezone: "UTC",
		},
		"empty name": {
			ProjectID: pid, Name: "", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "04:00", Timezone: "UTC",
		},
		// start_time == end_time проходило бы валидацию и трактовалось
		// windowDuration как 24-часовое окно (P2-3 из аудита 2026-08-12) —
		// опечатка при вводе времени молча блэкаутила бы весь день недели.
		"start equals end": {
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "02:00", Timezone: "UTC",
		},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.CreateWindow(ctx, w); !errors.Is(err, uptime.ErrInvalidWindow) {
				t.Fatalf("CreateWindow(%+v): err = %v, want ErrInvalidWindow", w, err)
			}
		})
	}
}

// TestCreateWindowInvalidFieldsSentinels — P2-1 usability-аудита 2026-08-12:
// validateWindow теперь оборачивает каждую причину своим сентинелем поверх
// общего ErrInvalidWindow (см. internal/uptime/maintenance.go), чтобы
// web-слой мог перевести конкретную причину без показа сырого err.Error().
// Проверяем, что каждая ветка даёт СВОЙ, а не общий, сентинель.
func TestCreateWindowInvalidFieldsSentinels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	cases := []struct {
		name string
		w    uptime.Window
		want error
	}{
		{"empty name", uptime.Window{
			ProjectID: pid, Name: "", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "04:00", Timezone: "UTC",
		}, uptime.ErrInvalidWindowName},
		{"bad timezone", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "04:00", Timezone: "Not/A_Zone",
		}, uptime.ErrInvalidWindowTimezone},
		{"bad weekday", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 7,
			StartTime: "02:00", EndTime: "04:00", Timezone: "UTC",
		}, uptime.ErrInvalidWindowWeekday},
		{"bad start time", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "25:00", EndTime: "04:00", Timezone: "UTC",
		}, uptime.ErrInvalidWindowStartTime},
		{"bad end time", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "not-a-time", Timezone: "UTC",
		}, uptime.ErrInvalidWindowEndTime},
		{"start equals end", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: true, Weekday: 1,
			StartTime: "02:00", EndTime: "02:00", Timezone: "UTC",
		}, uptime.ErrInvalidWindowSameTime},
		{"one-off missing range", uptime.Window{
			ProjectID: pid, Name: "x", Weekly: false, Timezone: "UTC",
		}, uptime.ErrInvalidWindowRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateWindow(ctx, c.w)
			if !errors.Is(err, uptime.ErrInvalidWindow) {
				t.Fatalf("CreateWindow(%+v): err = %v, want ErrInvalidWindow", c.w, err)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("CreateWindow(%+v): err = %v, want also matching %v", c.w, err, c.want)
			}
		})
	}
}
