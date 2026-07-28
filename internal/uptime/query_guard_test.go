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

// TestBarsBatchGuardBeforeAllocate: BarsBatch, как и Bars, отсекает buckets<=0
// ДО make([]UptimeStat, buckets) — иначе отрицательный buckets паникует в
// makeslice. Мис-копирование формы из UptimeBatch тихо сняло бы этот guard.
func TestBarsBatchGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn достаточно: вырожденный вход возвращается рано
	ctx := context.Background()
	now := time.Now().UTC()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BarsBatch with buckets=-1 panicked: %v", r)
		}
	}()

	out, err := q.BarsBatch(ctx, []int64{1, 2}, now.Add(-time.Hour), now, -1)
	if err != nil {
		t.Fatalf("BarsBatch with buckets=-1 returned error: %v", err)
	}
	// Вырожденный вход: как одиночный Bars — nil-срез на каждый монитор.
	for _, id := range []int64{1, 2} {
		if out[id] != nil {
			t.Fatalf("BarsBatch[%d] = %v, want nil on degenerate input", id, out[id])
		}
	}
}
