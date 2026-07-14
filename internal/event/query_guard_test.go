package event

import (
	"context"
	"testing"
	"time"
)

// TestSparklinesGuardBeforeAllocate verifies Sparklines validates buckets > 0 before allocating.
// Negative buckets would panic with "makeslice: len out of range" without the guard.
func TestSparklinesGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Sparklines returns early
	ctx := context.Background()

	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	// Test negative buckets: should not panic, should return empty or error-free
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Sparklines with buckets=-1 panicked: %v", r)
		}
	}()

	out, err := q.Sparklines(ctx, 1, []int64{10, 20}, since, -1)
	if err != nil {
		t.Fatalf("Sparklines with buckets=-1 returned error: %v", err)
	}
	// Even though issueIDs are provided, buckets <= 0 should return early without allocating
	if out != nil && len(out) != 0 {
		t.Fatalf("Sparklines with buckets=-1 returned non-empty result: %v", out)
	}
}
