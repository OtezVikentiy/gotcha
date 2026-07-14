package uptime

import (
	"context"
	"testing"
	"time"
)

// TestBarsGuardBeforeAllocate verifies Bars validates buckets > 0 before allocating.
// Negative buckets would panic with "makeslice: len out of range" without the guard.
func TestBarsGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Bars returns early
	ctx := context.Background()

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now

	// Test negative buckets: should not panic, should return empty or error-free
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Bars with buckets=-1 panicked: %v", r)
		}
	}()

	out, err := q.Bars(ctx, 1, from, to, -1)
	if err != nil {
		t.Fatalf("Bars with buckets=-1 returned error: %v", err)
	}
	if out != nil && len(out) != 0 {
		t.Fatalf("Bars with buckets=-1 returned non-empty result: %v", out)
	}
}

// TestRecentGuardBeforeAllocate verifies Recent validates limit > 0 before allocating.
// Negative limit would panic with "makeslice: len out of range" without the guard.
func TestRecentGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Recent returns early
	ctx := context.Background()

	// Test negative limit: should not panic, should return empty or error-free
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Recent with limit=-5 panicked: %v", r)
		}
	}()

	rows, err := q.Recent(ctx, 1, -5)
	if err != nil {
		t.Fatalf("Recent with limit=-5 returned error: %v", err)
	}
	if rows != nil && len(rows) != 0 {
		t.Fatalf("Recent with limit=-5 returned non-empty result: %v", rows)
	}
}
