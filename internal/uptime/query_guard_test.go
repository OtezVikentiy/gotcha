package uptime

import (
	"context"
	"testing"
	"time"
)

// ноль здесь не менее важен, чем отрицательное число: `<= 0`, ослабленный до
// `< 0`, пропускает ровно ноль — деление на ноль в Bars/BarsBatch, LIMIT 0 в Recent.
var degenerateCounts = []int{-1, 0}

func TestBarsGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Bars returns early
	ctx := context.Background()

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now

	for _, buckets := range degenerateCounts {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Bars with buckets=%d panicked: %v", buckets, r)
				}
			}()
			out, err := q.Bars(ctx, 1, from, to, buckets)
			if err != nil {
				t.Fatalf("Bars with buckets=%d returned error: %v", buckets, err)
			}
			if len(out) != 0 {
				t.Fatalf("Bars with buckets=%d returned non-empty result: %v", buckets, out)
			}
		}()
	}
}

func TestRecentGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn is sufficient; Recent returns early
	ctx := context.Background()

	for _, limit := range degenerateCounts {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Recent with limit=%d panicked: %v", limit, r)
				}
			}()
			rows, err := q.Recent(ctx, 1, limit)
			if err != nil {
				t.Fatalf("Recent with limit=%d returned error: %v", limit, err)
			}
			if len(rows) != 0 {
				t.Fatalf("Recent with limit=%d returned non-empty result: %v", limit, rows)
			}
		}()
	}
}

func TestBarsBatchGuardBeforeAllocate(t *testing.T) {
	q := NewQuery(nil) // nil conn достаточно: вырожденный вход возвращается рано
	ctx := context.Background()
	now := time.Now().UTC()

	for _, buckets := range degenerateCounts {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("BarsBatch with buckets=%d panicked: %v", buckets, r)
				}
			}()
			out, err := q.BarsBatch(ctx, []int64{1, 2}, now.Add(-time.Hour), now, buckets)
			if err != nil {
				t.Fatalf("BarsBatch with buckets=%d returned error: %v", buckets, err)
			}
			// Вырожденный вход: как одиночный Bars — nil-срез на каждый монитор.
			for _, id := range []int64{1, 2} {
				if out[id] != nil {
					t.Fatalf("BarsBatch[%d] with buckets=%d = %v, want nil on degenerate input", id, buckets, out[id])
				}
			}
		}()
	}
}
