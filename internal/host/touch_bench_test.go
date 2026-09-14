package host

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Карта seen заполнена до потолка, каждый Touch несёт одно новое имя -> одно вытеснение на
// вызов, под тем же t.mu, что и обычная регистрация.
func BenchmarkToucherEvictAtCapacity(b *testing.B) {
	const cap = 65536
	tc := NewToucher(nil, time.Hour, cap)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error { return nil }

	for i := 0; i < cap; i++ {
		tc.Touch(context.Background(), 1, entries(fmt.Sprintf("seed-%d", i)))
	}
	tc.wait()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tc.Touch(context.Background(), 1, entries(fmt.Sprintf("bench-%d", i)))
	}
	tc.wait()
}
