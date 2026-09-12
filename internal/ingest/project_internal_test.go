package ingest

import (
	"testing"
	"time"
)

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
