package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// TestCacheControl проверяет, что cacheControl проставляет заголовок
// Cache-Control: max-age=3600 на ответ и при этом реально вызывает
// обёрнутый handler (а не проглатывает запрос).
func TestCacheControl(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})

	wrapped := cacheControl(inner)

	req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if !called {
		t.Fatal("cacheControl не вызвал обёрнутый handler")
	}
	if got := rec.Header().Get("Cache-Control"); got != "max-age=3600" {
		t.Fatalf("Cache-Control = %q, want %q", got, "max-age=3600")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d (доказывает, что тело хендлера отработало)", rec.Code, http.StatusTeapot)
	}
}

// TestMonitorStatus покрывает все три ветки monitorStatus: paused (Enabled
// == false), maintenance (активное окно обслуживания) и делегирование в
// uptime.Aggregate для обычного случая.
func TestMonitorStatus(t *testing.T) {
	t.Run("disabled monitor is paused regardless of states", func(t *testing.T) {
		m := uptime.Monitor{Enabled: false, Consensus: uptime.ConsensusAny}
		states := []uptime.State{{Status: "down"}}
		got := monitorStatus(m, states, false)
		if got != "paused" {
			t.Fatalf("monitorStatus() = %q, want %q", got, "paused")
		}
	})

	t.Run("enabled monitor in maintenance window reports maintenance even when down", func(t *testing.T) {
		m := uptime.Monitor{Enabled: true, Consensus: uptime.ConsensusAny}
		states := []uptime.State{{Status: "down"}}
		got := monitorStatus(m, states, true)
		if got != "maintenance" {
			t.Fatalf("monitorStatus() = %q, want %q", got, "maintenance")
		}
	})

	t.Run("enabled monitor outside maintenance delegates to uptime.Aggregate", func(t *testing.T) {
		// Consensus=any, единственный регион в статусе "up" => decided=1,
		// down=0 => ConsensusAny не срабатывает (down>0 ложно) => aggUp =>
		// Aggregate возвращает буквально "up" (см. internal/uptime/detector.go).
		m := uptime.Monitor{Enabled: true, Consensus: uptime.ConsensusAny}
		states := []uptime.State{{Region: "eu", Status: "up"}}
		got := monitorStatus(m, states, false)
		if got != "up" {
			t.Fatalf("monitorStatus() = %q, want literal %q", got, "up")
		}
	})
}

// TestUpcomingWindows проверяет разворачивание окон обслуживания в
// StatusWindowView: пустой список окон, и несколько окон, пересекающих
// [from,to), с сортировкой результата по времени начала.
func TestUpcomingWindows(t *testing.T) {
	from := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	t.Run("no windows yields no views", func(t *testing.T) {
		got := upcomingWindows(nil, from, to)
		if len(got) != 0 {
			t.Fatalf("upcomingWindows() = %#v, want empty", got)
		}
	})

	t.Run("overlapping windows come back sorted by start time", func(t *testing.T) {
		earlyStart := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
		earlyEnd := time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC)
		lateStart := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
		lateEnd := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		// Окно за пределами [from,to) не должно попасть в результат.
		outsideStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		outsideEnd := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)

		windows := []uptime.Window{
			{Name: "Second window", StartsAt: &lateStart, EndsAt: &lateEnd},
			{Name: "First window", StartsAt: &earlyStart, EndsAt: &earlyEnd},
			{Name: "Out of range", StartsAt: &outsideStart, EndsAt: &outsideEnd},
		}

		got := upcomingWindows(windows, from, to)

		want := []templates.StatusWindowView{
			{Name: "First window", From: "2026-07-23 01:00 UTC", To: "2026-07-23 03:00 UTC"},
			{Name: "Second window", From: "2026-07-25 10:00 UTC", To: "2026-07-25 12:00 UTC"},
		}
		if len(got) != len(want) {
			t.Fatalf("upcomingWindows() = %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("upcomingWindows()[%d] = %#v, want %#v", i, got[i], want[i])
			}
		}
	})
}
