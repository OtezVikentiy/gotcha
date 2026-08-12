package ingest

import (
	"testing"
	"time"
)

// TestProjectCacheIsBounded — ProjectCache обязан ограничивать размер карты,
// как KeyCache/OrgQuota, а не расти без границы на инсталляции с очень
// большим числом проектов за долгий аптайм (P2-1 из аудита 2026-08-12).
func TestProjectCacheIsBounded(t *testing.T) {
	now := time.Unix(0, 0)
	c := &ProjectCache{
		entries: map[int64]projectEntry{},
		ttl:     time.Minute,
		now:     func() time.Time { return now },
	}

	for id := int64(0); id < maxKeyCacheEntries+100; id++ {
		c.mu.Lock()
		if len(c.entries) >= maxKeyCacheEntries {
			c.evict(now)
		}
		c.entries[id] = projectEntry{expires: now.Add(c.ttl)}
		c.mu.Unlock()
	}
	if len(c.entries) > maxKeyCacheEntries {
		t.Fatalf("записей %d, want <= %d — кеш проектов без границы", len(c.entries), maxKeyCacheEntries)
	}
	if len(c.entries) < maxKeyCacheEntries*8/10 {
		t.Fatalf("осталось %d записей из %d — похоже на полный сброс кеша", len(c.entries), maxKeyCacheEntries)
	}
}

// TestProjectCacheEvictsExpiredFirst — истёкшие записи должны вытесняться
// раньше живых, иначе поток проектов при переполнении выбивал бы заодно и
// свежие записи.
func TestProjectCacheEvictsExpiredFirst(t *testing.T) {
	now := time.Unix(0, 0)
	c := &ProjectCache{
		entries: map[int64]projectEntry{},
		ttl:     time.Minute,
		now:     func() time.Time { return now },
	}
	for id := int64(0); id < maxKeyCacheEntries; id++ {
		if id%2 == 0 {
			c.entries[id] = projectEntry{expires: now.Add(-time.Second)} // уже истекла
		} else {
			c.entries[id] = projectEntry{expires: now.Add(time.Hour)}
		}
	}
	c.mu.Lock()
	c.evict(now)
	c.entries[999999] = projectEntry{expires: now.Add(c.ttl)}
	c.mu.Unlock()

	for id, e := range c.entries {
		if id != 999999 && !e.expires.After(now) {
			t.Fatalf("истёкшая запись %d пережила вытеснение", id)
		}
	}
}
