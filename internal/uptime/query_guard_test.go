package uptime

import (
	"context"
	"testing"
	"time"
)

// degenerateCounts — вырожденные значения buckets/limit, которые guard'ы
// обязаны отсечь до аллокации/деления/запроса. Ноль здесь не менее важен,
// чем отрицательное число (K2-6/K2-7): мутация `<= 0` → `< 0` пропускает
// ровно ноль — в Bars/BarsBatch это целочисленное деление на ноль при
// расчёте ширины корзины, в Recent — уход в ClickHouse с LIMIT 0.
var degenerateCounts = []int{-1, 0}

// TestBarsGuardBeforeAllocate verifies Bars validates buckets > 0 before allocating.
// Negative buckets would panic with "makeslice: len out of range" without the guard;
// zero buckets would panic dividing the range by the bucket count.
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

// TestRecentGuardBeforeAllocate verifies Recent validates limit > 0 before allocating.
// Negative limit would panic with "makeslice: len out of range" without the guard;
// zero limit must return early too, not reach ClickHouse (nil conn would blow up).
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

// TestBarsBatchGuardBeforeAllocate: BarsBatch, как и Bars, отсекает buckets<=0
// ДО make([]UptimeStat, buckets) — иначе отрицательный buckets паникует в
// makeslice, а нулевой — в делении на число корзин. Мис-копирование формы из
// UptimeBatch тихо сняло бы этот guard.
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
