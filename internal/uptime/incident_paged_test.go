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

// TestIncidentsPagedOutOfRangeAndEmptyPage — контракт постраничных выборок на
// краях набора (см. докблок queryIncidentsPaged): страница внутри набора
// несёт total всего набора; offset за его пределами даёт nil-страницу и
// total=0 (ранний возврат без второго запроса); страница внутри набора, но
// без строк (limit=0 — LIMIT 0 в PG отдаёт пусто) тоже даёт nil и total=0 —
// ровно так себя вела однозапросная форма с count(*) OVER(), где total
// нечего было прочитать без строк, и на это опирается пейджер веб-слоя.
func TestIncidentsPagedOutOfRangeAndEmptyPage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	// Два инцидента на одном мониторе: открыть → закрыть → открыть.
	if _, _, err := svc.OpenIncident(ctx, created.ID, "first", []string{"local"}, false); err != nil {
		t.Fatalf("OpenIncident 1: %v", err)
	}
	if _, ok, err := svc.ResolveIncident(ctx, created.ID, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("ResolveIncident: (%v,%v)", ok, err)
	}
	if _, _, err := svc.OpenIncident(ctx, created.ID, "second", []string{"local"}, false); err != nil {
		t.Fatalf("OpenIncident 2: %v", err)
	}

	type pager func(limit, offset int) ([]uptime.Incident, int64, error)
	pagers := map[string]pager{
		"IncidentsPaged": func(limit, offset int) ([]uptime.Incident, int64, error) {
			return svc.IncidentsPaged(ctx, pid, limit, offset)
		},
		"IncidentsForMonitorPaged": func(limit, offset int) ([]uptime.Incident, int64, error) {
			return svc.IncidentsForMonitorPaged(ctx, created.ID, limit, offset)
		},
	}
	cases := []struct {
		name          string
		limit, offset int
		wantRows      int
		wantTotal     int64
	}{
		{"page 1 of 2", 1, 0, 1, 2},
		{"page 2 of 2", 1, 1, 1, 2},
		{"offset == total", 1, 2, 0, 0},
		{"offset far beyond total", 10, 100, 0, 0},
		{"limit 0 inside the set", 0, 0, 0, 0},
	}
	for who, page := range pagers {
		for _, tc := range cases {
			rows, total, err := page(tc.limit, tc.offset)
			if err != nil {
				t.Fatalf("%s %s: %v", who, tc.name, err)
			}
			if tc.wantRows == 0 && rows != nil {
				t.Errorf("%s %s: rows = %v, want nil page", who, tc.name, rows)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("%s %s: rows = %d, want %d", who, tc.name, len(rows), tc.wantRows)
			}
			if total != tc.wantTotal {
				t.Errorf("%s %s: total = %d, want %d", who, tc.name, total, tc.wantTotal)
			}
		}
	}
}
