package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestIncidentsPagedReturnNonEmptyPage — постраничные выборки инцидентов
// (обе страницы UI: список инцидентов проекта и деталь монитора) обязаны
// отдавать НЕПУСТУЮ страницу.
//
// Регрессия, ради которой тест заведён: очередная колонка (notify_open_channels,
// миграция 0086) была добавлена в incidentColumns и в scanIncident, но не в
// список приёмников внутри queryIncidentsPaged — второй, независимый список
// на ту же строку колонок. Пустая выборка при этом остаётся зелёной: pgx
// сверяет число приёмников с числом колонок только при разборе СТРОКИ,
// поэтому проверка обязана сажать инцидент в базу, а не только звать запрос.
func TestIncidentsPagedReturnNonEmptyPage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	inc, opened, err := svc.OpenIncident(ctx, created.ID, "connection refused", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !opened {
		t.Fatalf("OpenIncident: инцидент не создан")
	}
	// Снимок каналов шага 0 — та самая колонка, из-за которой списки падали;
	// проверяем её значение, а не только отсутствие ошибки, чтобы выпадение
	// колонки из выборки роняло тест, а не проходило молча.
	if err := svc.SetNotifyOpenChannels(ctx, inc.ID, []int64{7, 9}); err != nil {
		t.Fatalf("SetNotifyOpenChannels: %v", err)
	}

	page, total, err := svc.IncidentsPaged(ctx, pid, 10, 0)
	if err != nil {
		t.Fatalf("IncidentsPaged: %v", err)
	}
	assertSingleIncident(t, "IncidentsPaged", page, total, inc.ID)

	mpage, mtotal, err := svc.IncidentsForMonitorPaged(ctx, created.ID, 10, 0)
	if err != nil {
		t.Fatalf("IncidentsForMonitorPaged: %v", err)
	}
	assertSingleIncident(t, "IncidentsForMonitorPaged", mpage, mtotal, inc.ID)
}

func assertSingleIncident(t *testing.T, who string, page []uptime.Incident, total, wantID int64) {
	t.Helper()
	if len(page) != 1 {
		t.Fatalf("%s: инцидентов на странице = %d, ожидалось 1", who, len(page))
	}
	if total != 1 {
		t.Errorf("%s: total = %d, ожидалось 1", who, total)
	}
	if page[0].ID != wantID {
		t.Errorf("%s: id инцидента = %d, ожидалось %d", who, page[0].ID, wantID)
	}
	if got := page[0].NotifyOpenChannels; len(got) != 2 || got[0] != 7 || got[1] != 9 {
		t.Errorf("%s: NotifyOpenChannels = %v, ожидалось [7 9]", who, got)
	}
}
