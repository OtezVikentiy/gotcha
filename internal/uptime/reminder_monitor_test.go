package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// напоминание собирает Monitor отдельным списком колонок, не monitorColumns —
// рассинхрон полей (например retries) не поймает компилятор, только тест.
func TestReminderCarriesFullMonitor(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	m.RemindEveryMinutes = 1
	m.Retries = 3
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// notified_open=true обязателен: иначе гейт по нему исключит инцидент из
	// выборки, и тест проверял бы пустоту.
	if _, err := pool.Exec(ctx,
		`INSERT INTO incidents (monitor_id, started_at, cause, notified_open) VALUES ($1, now() - interval '1 hour', 'down', true)`,
		created.ID); err != nil {
		t.Fatalf("insert incident: %v", err)
	}

	items, err := svc.IncidentsDueForReminder(ctx)
	if err != nil {
		t.Fatalf("IncidentsDueForReminder: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.Monitor.ID != created.ID {
			continue
		}
		found = true
		if it.Monitor.Retries != 3 {
			t.Errorf("Monitor.Retries = %d, want 3: монитор собран не целиком", it.Monitor.Retries)
		}
		if it.Monitor.Name != created.Name || it.Monitor.ProjectID != pid {
			t.Errorf("монитор собран неверно: %+v", it.Monitor)
		}
	}
	if !found {
		t.Fatal("инцидент не попал в список напоминаний — тест проверял бы пустоту")
	}
}

// кроме due по remind_every_minutes, инцидент обязан быть notified_open
// (подавленным/удержанным грейсом ещё не наступил) и не suppressed_by_dep.
func TestIncidentsDueForReminderGate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	newMonitor := func() uptime.Monitor {
		m := baseHTTPMonitor(pid)
		m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
		m.RemindEveryMinutes = 1
		created, err := svc.Create(ctx, m, []string{"local"}, nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return created
	}
	// 10 минут при remind_every=1 — заведомо просрочено; единственный барьер —
	// сам гейт по notified_open/suppressed_by_dep.
	insertIncident := func(monitorID int64, notifiedOpen, suppressed bool) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO incidents (monitor_id, started_at, cause, notified_open, suppressed_by_dep)
			VALUES ($1, now() - interval '10 minutes', 'down', $2, $3)
			RETURNING id`, monitorID, notifiedOpen, suppressed).Scan(&id); err != nil {
			t.Fatalf("insert incident: %v", err)
		}
		return id
	}

	monNotOpen := newMonitor()
	monEligible := newMonitor()
	monSuppressed := newMonitor()

	idNotOpen := insertIncident(monNotOpen.ID, false, false)
	idEligible := insertIncident(monEligible.ID, true, false)
	idSuppressed := insertIncident(monSuppressed.ID, true, true)

	items, err := svc.IncidentsDueForReminder(ctx)
	if err != nil {
		t.Fatalf("IncidentsDueForReminder: %v", err)
	}
	got := make(map[int64]bool, len(items))
	for _, it := range items {
		got[it.Incident.ID] = true
	}
	if got[idNotOpen] {
		t.Errorf("incident %d: notified_open=false присутствует в выборке напоминаний, ожидалось исключение", idNotOpen)
	}
	if !got[idEligible] {
		t.Errorf("incident %d: notified_open=true suppressed_by_dep=false отсутствует в выборке напоминаний, ожидалось включение", idEligible)
	}
	if got[idSuppressed] {
		t.Errorf("incident %d: suppressed_by_dep=true присутствует в выборке напоминаний, ожидалось исключение", idSuppressed)
	}
}
