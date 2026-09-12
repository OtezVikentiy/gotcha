package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

func TestAttrKeysCache(t *testing.T) {
	now := time.Now()
	c := newAttrKeysCache()
	c.now = func() time.Time { return now }

	if _, hit := c.get(1, "http.", "24h", "", ""); hit {
		t.Fatalf("пустой кеш не должен давать хит")
	}

	values := []log.FacetValue{{Value: "http.method", Count: 3}}
	c.put(1, "http.", "24h", "", "", values)

	got, hit := c.get(1, "http.", "24h", "", "")
	if !hit {
		t.Fatalf("после put — промах")
	}
	if len(got) != 1 || got[0].Value != "http.method" || got[0].Count != 3 {
		t.Fatalf("get вернул %+v, хотим %+v", got, values)
	}

	if _, hit := c.get(1, "db.", "24h", "", ""); hit {
		t.Fatalf("другой prefix не должен давать хит по записи \"http.\"")
	}
	if _, hit := c.get(2, "http.", "24h", "", ""); hit {
		t.Fatalf("другой projectID не должен давать хит по записи проекта 1")
	}

	now = now.Add(attrKeysCacheTTL - time.Second)
	if _, hit := c.get(1, "http.", "24h", "", ""); !hit {
		t.Fatalf("в пределах TTL должен быть хит")
	}

	now = now.Add(2 * time.Second)
	if _, hit := c.get(1, "http.", "24h", "", ""); hit {
		t.Fatalf("после истечения TTL должен быть промах")
	}
}

func TestAttrKeysCacheWindowIsolation(t *testing.T) {
	now := time.Now()
	c := newAttrKeysCache()
	c.now = func() time.Time { return now }

	c.put(1, "http.", "1h", "", "", []log.FacetValue{{Value: "http.method", Count: 1}})

	if _, hit := c.get(1, "http.", "7d", "", ""); hit {
		t.Fatalf("другой period не должен давать хит по записи окна \"1h\"")
	}
	if _, hit := c.get(1, "http.", "", "2026-01-01", "2026-01-02"); hit {
		t.Fatalf("произвольный диапазон не должен давать хит по записи пресета \"1h\"")
	}
	if _, hit := c.get(1, "http.", "1h", "", ""); !hit {
		t.Fatalf("то же окно \"1h\" должно остаться хитом")
	}
}

func TestAttrKeysCacheOverflowClearsAll(t *testing.T) {
	now := time.Now()
	c := newAttrKeysCache()
	c.now = func() time.Time { return now }

	for i := 0; i < maxAttrKeysCacheEntries; i++ {
		c.put(int64(i), "p", "24h", "", "", nil)
	}
	if len(c.entries) != maxAttrKeysCacheEntries {
		t.Fatalf("entries = %d, want %d перед переполнением", len(c.entries), maxAttrKeysCacheEntries)
	}

	c.put(999999, "p", "24h", "", "", nil)
	if len(c.entries) != 1 {
		t.Fatalf("entries = %d после переполнения, want 1 (только новая запись)", len(c.entries))
	}
	if _, hit := c.get(0, "p", "24h", "", ""); hit {
		t.Fatalf("старая запись должна была вытесниться при переполнении")
	}
	if _, hit := c.get(999999, "p", "24h", "", ""); !hit {
		t.Fatalf("новая запись, вызвавшая вытеснение, должна остаться в кеше")
	}
}
