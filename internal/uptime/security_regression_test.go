package uptime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestHTTPCheckerRedirectToPrivateIsBlocked (audit H5) — a follow-redirects
// HTTP monitor pointed at a server that 3xx-redirects into the internal
// network must NOT connect to the redirect target. netguard's per-hop
// DialContext is the guard: every hop (including each redirect) opens a fresh
// connection through the same guarded dialer, so a private/loopback Location
// is refused before any bytes are exchanged with it.
//
// Limitation worth stating plainly: httptest.Server binds to loopback
// (127.0.0.1), which netguard also blocks when AllowPrivate=false, so with the
// guard ON the *first* hop is already refused and the redirect handler never
// runs. That still proves the security outcome we care about — the private
// redirect target is never dialed — but it does not, on its own, isolate the
// per-redirect-hop guard from the first-hop guard (no public IP is bindable in
// the test env to serve an "allowed" first hop). The AllowPrivate=true control
// below pins that redirects ARE followed to that same target when the guard is
// off, so the only thing standing between the monitor and the internal target
// is netguard.
func TestHTTPCheckerRedirectToPrivateIsBlocked(t *testing.T) {
	// Redirect target: a loopback listener that must never be dialed by a
	// guarded checker. atomic flag records whether it ever served a request.
	var targetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("internal-secret"))
	}))
	defer target.Close()

	// First hop: 302-redirects into the (private/loopback) target.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer front.Close()

	cfg := httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             front.URL,
		FollowRedirects: true,
	})

	// Guard ON: the check must fail with a "blocked" error and the internal
	// target must never be reached.
	blocked := uptime.NewHTTPChecker(false)
	got := blocked.Check(context.Background(), checkerMonitor(uptime.KindHTTP, 5, cfg))
	if got.OK {
		t.Fatalf("Check() = %+v, want down (redirect into private network blocked)", got)
	}
	if !strings.Contains(got.Error, "blocked") {
		t.Errorf("Error = %q, want it to mention blocked target", got.Error)
	}
	if targetHit.Load() {
		t.Error("guarded checker connected to the private redirect target — SSRF")
	}

	// Positive control, guard OFF: redirects are genuinely followed to that
	// same target, so the block above is netguard's doing, not a broken chain.
	targetHit.Store(false)
	allowed := uptime.NewHTTPChecker(true)
	got = allowed.Check(context.Background(), checkerMonitor(uptime.KindHTTP, 5, cfg))
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() with AllowPrivate=true = %+v, want OK (redirect followed)", got)
	}
	if !targetHit.Load() {
		t.Error("AllowPrivate=true did not follow the redirect to the target; control is meaningless")
	}
}

// TestAggregateEvenRegionTie (audit H6) — фиксирует разрешение чётной ничьей по
// регионам (2 down / 2 up) для каждого режима консенсуса. Ничья под majority
// разрешается fail-safe в "down" (см. detector.go, `down*2 >= decided`): для
// инструмента мониторинга пропустить недоступность половины флота хуже, чем
// поднять инцидент на ничьей. Тест ловит регресс `>=`→`>` в detector.go.
func TestAggregateEvenRegionTie(t *testing.T) {
	states := []uptime.State{
		{Region: "r1", Status: "down"},
		{Region: "r2", Status: "down"},
		{Region: "r3", Status: "up"},
		{Region: "r4", Status: "up"},
	}
	cases := []struct {
		consensus uptime.Consensus
		want      string
	}{
		// any: a single down region is enough → down.
		{uptime.ConsensusAny, "down"},
		// all: not every region is down → up.
		{uptime.ConsensusAll, "up"},
		// majority: 2 из 4 down — ничья, fail-safe → down (down*2 >= decided).
		{uptime.ConsensusMajority, "down"},
	}
	for _, tc := range cases {
		m := uptime.Monitor{Consensus: tc.consensus}
		if got := uptime.Aggregate(m, states); got != tc.want {
			t.Errorf("Aggregate(consensus=%v, 2down/2up) = %q, want %q (current behavior)",
				tc.consensus, got, tc.want)
		}
	}
}
