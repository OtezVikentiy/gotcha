package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func TestCacheControl(t *testing.T) {
	const version = "abc123def456"
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"no version → short cache", "", "max-age=3600"},
		{"matching version → immutable", "?v=" + version, "public, max-age=31536000, immutable"},
		{"stale/foreign version → short cache", "?v=deadbeef", "max-age=3600"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			called := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusTeapot)
			})
			wrapped := cacheControl(version, inner)
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.css"+c.query, nil))
			if !called {
				t.Fatal("cacheControl не вызвал обёрнутый handler")
			}
			if got := rec.Header().Get("Cache-Control"); got != c.want {
				t.Fatalf("Cache-Control = %q, want %q", got, c.want)
			}
			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
			}
		})
	}
}

func TestNoDirListing(t *testing.T) {
	cases := []struct {
		path       string
		wantServed bool
	}{
		{"", false},
		{"icons/", false},
		{"app.css", true},
	}
	for _, c := range cases {
		served := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = true })
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.URL.Path = c.path
		noDirListing(inner).ServeHTTP(rec, req)
		if served != c.wantServed {
			t.Errorf("path %q: served=%v, want %v", c.path, served, c.wantServed)
		}
		if !c.wantServed && rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status=%d, want 404", c.path, rec.Code)
		}
	}
}

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
		m := uptime.Monitor{Enabled: true, Consensus: uptime.ConsensusAny}
		states := []uptime.State{{Region: "eu", Status: "up"}}
		got := monitorStatus(m, states, false)
		if got != "up" {
			t.Fatalf("monitorStatus() = %q, want literal %q", got, "up")
		}
	})
}

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
