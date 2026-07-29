package ingest

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestOrgQuotaCachesAreBounded — обе карты кеша квот обязаны иметь границу и
// вытеснять записи, а не расти без предела и не стираться целиком.
//
// entries раньше не имел границы вообще: записи только перезаписывались, по
// истечении TTL не удалялись, и карта росла до числа организаций, когда-либо
// приходивших на приём. exhausted при переполнении стирался целиком — учёт от
// этого не ломался, но выбрасывались ровно те записи, ради которых кеш и
// заведён, и организации, стабильно упирающиеся в квоту, снова шли в PostgreSQL
// на каждом событии.
func TestOrgQuotaCachesAreBounded(t *testing.T) {
	now := time.Unix(0, 0)
	q := &OrgQuota{
		entries:     map[int64]quotaEntry{},
		exhausted:   map[int64]time.Time{},
		ttl:         time.Minute,
		quotaNegTTL: time.Minute,
		now:         func() time.Time { return now },
	}

	t.Run("exhausted вытесняет, а не стирается", func(t *testing.T) {
		for id := int64(0); id < maxKeyCacheEntries+100; id++ {
			q.markExhausted(id)
		}
		if len(q.exhausted) > maxKeyCacheEntries {
			t.Fatalf("записей %d, want <= %d — граница не работает", len(q.exhausted), maxKeyCacheEntries)
		}
		if len(q.exhausted) < maxKeyCacheEntries*8/10 {
			t.Fatalf("осталось %d записей из %d — похоже на полный сброс кеша",
				len(q.exhausted), maxKeyCacheEntries)
		}
	})

	t.Run("истёкшие уходят первыми", func(t *testing.T) {
		q.exhausted = map[int64]time.Time{}
		// Половина записей уже протухла.
		for id := int64(0); id < maxKeyCacheEntries; id++ {
			if id%2 == 0 {
				q.exhausted[id] = now.Add(-time.Second)
			} else {
				q.exhausted[id] = now.Add(time.Hour)
			}
		}
		q.markExhausted(999999)
		for id, exp := range q.exhausted {
			if id != 999999 && !exp.After(now) {
				t.Fatalf("истёкшая запись %d пережила вытеснение", id)
			}
		}
	})

	t.Run("entries ограничен", func(t *testing.T) {
		for id := int64(0); id < maxKeyCacheEntries+100; id++ {
			q.mu.Lock()
			if len(q.entries) >= maxKeyCacheEntries {
				q.evictEntries(now)
			}
			q.entries[id] = quotaEntry{quota: 1, expires: now.Add(q.ttl)}
			q.mu.Unlock()
		}
		if len(q.entries) > maxKeyCacheEntries {
			t.Fatalf("записей %d, want <= %d — кеш квот без границы",
				len(q.entries), maxKeyCacheEntries)
		}
	})
}

// TestOrgQuotaUsesInjectedClock — CheckAndCount обязан считать месячное окно по
// ИНЖЕКТИРУЕМЫМ часам, а не по time.Now().
//
// Раньше в единственном месте, где определяется граница месяца, стояло
// `time.Now()`, поэтому поведение «квота обнулилась 1-го числа» было
// непроверяемо в принципе — для биллинговой логики это дорого. Тест ловит
// возврат к реальным часам: он двигает часы через границу месяца и смотрит,
// какой момент дошёл до checkCount.
func TestOrgQuotaUsesInjectedClock(t *testing.T) {
	var now time.Time
	var seen []time.Time
	q := &OrgQuota{
		svc:         &fakeQuotaResolver{quota: 1000},
		ttl:         time.Hour,
		quotaNegTTL: time.Hour,
		now:         func() time.Time { return now },
		quotaOf:     func(o org.Org) int64 { return o.EventQuota },
		checkCount: func(_ context.Context, _ int64, at time.Time, _, want int64) (int64, error) {
			seen = append(seen, at)
			return want, nil
		},
		entries:   map[int64]quotaEntry{},
		exhausted: map[int64]time.Time{},
	}
	ctx := context.Background()

	now = time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)
	if granted, err := q.CheckAndCount(ctx, 7, 1); err != nil || granted != 1 {
		t.Fatalf("январь: granted=%v err=%v", granted, err)
	}
	now = time.Date(2026, time.February, 1, 0, 0, 1, 0, time.UTC)
	if granted, err := q.CheckAndCount(ctx, 7, 1); err != nil || granted != 1 {
		t.Fatalf("февраль: granted=%v err=%v", granted, err)
	}

	if len(seen) != 2 {
		t.Fatalf("checkCount вызван %d раз, want 2", len(seen))
	}
	// Ключевая проверка: до checkCount доехали ИМЕННО инжектированные моменты.
	// С time.Now() оба были бы «сейчас» и попали бы в один и тот же месяц.
	if !seen[0].Equal(time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("первый вызов получил %v, ожидались инжектированные часы", seen[0])
	}
	if seen[0].Month() == seen[1].Month() {
		t.Errorf("оба вызова попали в %v — граница месяца не переехала (часы реальные?)", seen[0].Month())
	}
}
