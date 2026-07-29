package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestUpdateWindowShiftsSchedule — самая частая правка окна: сдвинуть его на
// час. Проверяем не только запись полей, но и то, что детектор после правки
// считает даунтаймом ровно новый интервал: окна существуют затем, чтобы
// InMaintenance отвечал по ним, а не ради строки в таблице.
func TestUpdateWindowShiftsSchedule(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	start := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid, Name: "Deploy", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	newStart := start.Add(time.Hour)
	newEnd := end.Add(time.Hour)
	if err := svc.UpdateWindow(ctx, uptime.Window{
		ID: w.ID, ProjectID: pid, Name: "Deploy (сдвинуто)",
		StartsAt: &newStart, EndsAt: &newEnd, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("UpdateWindow: %v", err)
	}

	// Начало прежнего окна больше не обслуживание, начало нового — да.
	if active, err := svc.InMaintenance(ctx, pid, start.Add(30*time.Minute)); err != nil || active {
		t.Fatalf("InMaintenance at old start = %v err=%v, want false", active, err)
	}
	if active, err := svc.InMaintenance(ctx, pid, newEnd.Add(-30*time.Minute)); err != nil || !active {
		t.Fatalf("InMaintenance at new end = %v err=%v, want true", active, err)
	}

	ws, err := svc.Windows(ctx, pid)
	if err != nil || len(ws) != 1 || ws[0].Name != "Deploy (сдвинуто)" {
		t.Fatalf("Windows after update = %+v err=%v", ws, err)
	}
}

// TestUpdateWindowSwitchesKind — переключение разового окна в еженедельное.
// Колонки другого вида расписания обнуляются: окно со свежим weekday и старым
// starts_at не прошло бы ни validateWindow, ни windowActive.
func TestUpdateWindowSwitchesKind(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	start := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC) // понедельник
	end := start.Add(2 * time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid, Name: "Deploy", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	if err := svc.UpdateWindow(ctx, uptime.Window{
		ID: w.ID, ProjectID: pid, Name: "Nightly", Weekly: true,
		Weekday: 2, StartTime: "01:00", EndTime: "02:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("UpdateWindow: %v", err)
	}

	ws, err := svc.Windows(ctx, pid)
	if err != nil || len(ws) != 1 {
		t.Fatalf("Windows = %+v err=%v", ws, err)
	}
	got := ws[0]
	if !got.Weekly || got.Weekday != 2 || got.StartTime != "01:00" || got.EndTime != "02:00" {
		t.Fatalf("window after switch = %+v, want weekly Tue 01:00–02:00", got)
	}
	if got.StartsAt != nil || got.EndsAt != nil {
		t.Fatalf("one-off columns not cleared: starts=%v ends=%v", got.StartsAt, got.EndsAt)
	}

	// Прежний разовый интервал больше не обслуживание, новый еженедельный — да.
	if active, err := svc.InMaintenance(ctx, pid, start.Add(time.Hour)); err != nil || active {
		t.Fatalf("InMaintenance in old one-off = %v err=%v, want false", active, err)
	}
	tue := time.Date(2026, 7, 14, 1, 30, 0, 0, time.UTC)
	if active, err := svc.InMaintenance(ctx, pid, tue); err != nil || !active {
		t.Fatalf("InMaintenance in new weekly = %v err=%v, want true", active, err)
	}
}

// TestUpdateWindowScopedToProject — id окна приходит из формы, поэтому
// project_id стоит в условии UPDATE: иначе владелец одного проекта переписал бы
// окно соседнего, подобрав id.
func TestUpdateWindowScopedToProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	theirs := newProject(t, pool)
	mine := newProject(t, pool)

	start := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: theirs, Name: "Deploy", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	otherStart := start.Add(72 * time.Hour)
	otherEnd := otherStart.Add(time.Hour)
	err = svc.UpdateWindow(ctx, uptime.Window{
		ID: w.ID, ProjectID: mine, Name: "Hijacked",
		StartsAt: &otherStart, EndsAt: &otherEnd, Timezone: "UTC",
	})
	if !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("UpdateWindow across projects = %v, want ErrNotFound", err)
	}

	ws, err := svc.Windows(ctx, theirs)
	if err != nil || len(ws) != 1 || ws[0].Name != "Deploy" {
		t.Fatalf("victim window = %+v err=%v, want untouched", ws, err)
	}
}

// TestUpdateWindowValidates — правка проходит ту же валидацию, что и создание,
// иначе окно можно было бы испортить в обход проверок.
func TestUpdateWindowValidates(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	start := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid, Name: "Deploy", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	// Конец раньше начала — то же, что отклоняет CreateWindow.
	badEnd := start.Add(-time.Hour)
	err = svc.UpdateWindow(ctx, uptime.Window{
		ID: w.ID, ProjectID: pid, Name: "Deploy", StartsAt: &start, EndsAt: &badEnd, Timezone: "UTC",
	})
	if !errors.Is(err, uptime.ErrInvalidWindow) {
		t.Fatalf("UpdateWindow with end before start = %v, want ErrInvalidWindow", err)
	}
}
