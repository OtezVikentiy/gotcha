package web

import (
	"testing"
	"time"
)

func TestRateLimiterSweepExpiredEntries(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	rl := newRateLimiter(clock, 5, 10*time.Second)

	// Insert 10001 distinct keys to trigger sweep
	for i := 0; i < 10001; i++ {
		key := key(i)
		rl.Allow(key)
	}

	initialSize := rl.size()
	if initialSize != 10001 {
		t.Errorf("expected 10001 entries after insertion, got %d", initialSize)
	}

	// Advance clock past the window
	now = now.Add(15 * time.Second)

	// Trigger one more Allow() call to invoke the sweep
	rl.Allow("trigger_sweep")

	finalSize := rl.size()
	if finalSize >= 100 {
		t.Errorf("expected map size to drop significantly after sweep, got %d (should be < 100)", finalSize)
	}

	// Verify that the newly added entry is still there
	if finalSize == 0 {
		t.Errorf("expected at least the newly added 'trigger_sweep' entry, got 0 entries")
	}
}

func key(i int) string {
	// Generate a distinct key for each iteration
	return "192.168.1.1|test" + string(rune(i))
}
