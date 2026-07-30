package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestReminderCarriesFullMonitor: напоминание об открытом инциденте несёт
// монитор целиком, включая retries.
//
// Запрос собирал Monitor своим списком колонок вместо monitorColumns и retries
// пропускал: поле молча оставалось нулевым. Сегодня напоминание им не
// пользуется, но два способа собрать один объект — это заготовка расхождения,
// и замечает его тот, кто добавит зависимость от поля.
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

	// Инцидент, открытый достаточно давно, чтобы напоминание стало нужным.
	if _, err := pool.Exec(ctx,
		`INSERT INTO incidents (monitor_id, started_at, cause) VALUES ($1, now() - interval '1 hour', 'down')`,
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
