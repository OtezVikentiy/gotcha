package ingest

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// fakeQuotaResolver — заглушка quotaResolver: Get отдаёт фиксированную квоту и
// считает обращения (ими проверяем, что негативный кеш экономит поход в PG).
type fakeQuotaResolver struct {
	quota    int64
	getCalls int
}

func (f *fakeQuotaResolver) Get(context.Context, int64) (org.Org, error) {
	f.getCalls++
	return org.Org{EventQuota: f.quota}, nil
}
func (f *fakeQuotaResolver) CheckAndCountEvents(context.Context, int64, time.Time, int64) (bool, error) {
	return true, nil
}
func (f *fakeQuotaResolver) CheckAndCountTransactions(context.Context, int64, time.Time, int64) (bool, error) {
	return true, nil
}
func (f *fakeQuotaResolver) CheckAndCountMetrics(context.Context, int64, time.Time, int64) (bool, error) {
	return true, nil
}
func (f *fakeQuotaResolver) CheckAndCountProfiles(context.Context, int64, time.Time, int64) (bool, error) {
	return true, nil
}

// TestQuotaNegativeCacheShortCircuits: после первого over-quota повторные
// проверки той же орги в пределах TTL НЕ ходят в PG (ни checkCount, ни Get).
func TestQuotaNegativeCacheShortCircuits(t *testing.T) {
	now := time.Unix(1000, 0)
	fake := &fakeQuotaResolver{quota: 1}
	checkCalls := 0
	q := &OrgQuota{
		svc:         fake,
		ttl:         30 * time.Second,
		quotaNegTTL: 5 * time.Second,
		now:         func() time.Time { return now },
		quotaOf:     func(o org.Org) int64 { return o.EventQuota },
		checkCount: func(context.Context, int64, time.Time, int64) (bool, error) {
			checkCalls++
			return false, nil // всегда over-quota
		},
		entries:   map[int64]quotaEntry{},
		exhausted: map[int64]time.Time{},
	}
	ctx := context.Background()

	// Первый вызов реально ходит в PG (Get + checkCount) и кладёт негатив.
	if allowed, err := q.CheckAndCount(ctx, 7); err != nil || allowed {
		t.Fatalf("first: allowed=%v err=%v, want false/nil", allowed, err)
	}
	if checkCalls != 1 || fake.getCalls != 1 {
		t.Fatalf("first: checkCalls=%d getCalls=%d, want 1/1", checkCalls, fake.getCalls)
	}

	// Следующие 3 в пределах TTL — из кеша, без PG.
	for i := 0; i < 3; i++ {
		if allowed, _ := q.CheckAndCount(ctx, 7); allowed {
			t.Fatalf("cached call %d allowed, want denied", i)
		}
	}
	if checkCalls != 1 || fake.getCalls != 1 {
		t.Fatalf("cached: checkCalls=%d getCalls=%d, want still 1/1", checkCalls, fake.getCalls)
	}

	// За пределами TTL кеш протухает — снова идём в PG.
	now = now.Add(6 * time.Second)
	if allowed, _ := q.CheckAndCount(ctx, 7); allowed {
		t.Fatal("after TTL: allowed, want denied")
	}
	if checkCalls != 2 {
		t.Fatalf("after TTL: checkCalls=%d, want 2", checkCalls)
	}
}

// TestQuotaPositiveNeverCached: разрешённый приём НЕ кешируется — счётчик usage
// обязан расти на каждый принятый item, поэтому checkCount зовётся всякий раз.
func TestQuotaPositiveNeverCached(t *testing.T) {
	now := time.Unix(2000, 0)
	fake := &fakeQuotaResolver{quota: 100}
	calls := 0
	q := &OrgQuota{
		svc:         fake,
		ttl:         30 * time.Second,
		quotaNegTTL: 5 * time.Second,
		now:         func() time.Time { return now },
		quotaOf:     func(o org.Org) int64 { return o.EventQuota },
		checkCount: func(context.Context, int64, time.Time, int64) (bool, error) {
			calls++
			return true, nil
		},
		entries:   map[int64]quotaEntry{},
		exhausted: map[int64]time.Time{},
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if allowed, err := q.CheckAndCount(ctx, 7); err != nil || !allowed {
			t.Fatalf("call %d: allowed=%v err=%v, want true/nil", i, allowed, err)
		}
	}
	if calls != 5 {
		t.Fatalf("checkCount calls = %d, want 5 (positive never cached)", calls)
	}
}
