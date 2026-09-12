package event

import (
	"context"
	"testing"
	"time"
)

func TestSparklinesGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Sparklines returns early
	ctx := context.Background()

	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Sparklines with buckets=-1 panicked: %v", r)
		}
	}()

	out, err := q.Sparklines(ctx, 1, []int64{10, 20}, since, -1)
	if err != nil {
		t.Fatalf("Sparklines with buckets=-1 returned error: %v", err)
	}
	if out != nil && len(out) != 0 {
		t.Fatalf("Sparklines with buckets=-1 returned non-empty result: %v", out)
	}
}
