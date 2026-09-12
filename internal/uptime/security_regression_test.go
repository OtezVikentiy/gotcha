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

// httptest слушает loopback, которое netguard блокирует и на первом хопе,
// так что это не изолирует именно редирект-хоп, но исход (SSRF не пройдёт) верен.
func TestHTTPCheckerRedirectToPrivateIsBlocked(t *testing.T) {
	var targetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("internal-secret"))
	}))
	defer target.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer front.Close()

	cfg := httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             front.URL,
		FollowRedirects: true,
	})

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

	// guard OFF контролирует, что редирект вообще работает — иначе блокировка
	// выше могла быть просто сломанной цепочкой, а не заслугой netguard.
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

// ничья под majority — fail-safe в down (down*2 >= decided): пропустить
// недоступность половины флота хуже, чем поднять инцидент зря.
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
