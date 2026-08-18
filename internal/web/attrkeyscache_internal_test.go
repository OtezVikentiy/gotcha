package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

// TestAttrKeysCache закрепляет поведение кеша автокомплита ключей атрибутов
// (задача 6, C2, §6 спеки: «кеш per-project ~60с»): промах → put → хит в
// пределах TTL → истечение TTL → снова промах, ключ — (projectID, prefix,
// окно), другой prefix того же проекта не путается с первым. Тот же приём
// инъекции часов, что и TestKeyCache (internal/ingest/auth_test.go).
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

	// Другой prefix того же проекта — отдельная запись кеша, не хит.
	if _, hit := c.get(1, "db.", "24h", "", ""); hit {
		t.Fatalf("другой prefix не должен давать хит по записи \"http.\"")
	}
	// Тот же prefix другого проекта — тоже отдельная запись.
	if _, hit := c.get(2, "http.", "24h", "", ""); hit {
		t.Fatalf("другой projectID не должен давать хит по записи проекта 1")
	}

	// Чуть меньше TTL — всё ещё хит.
	now = now.Add(attrKeysCacheTTL - time.Second)
	if _, hit := c.get(1, "http.", "24h", "", ""); !hit {
		t.Fatalf("в пределах TTL должен быть хит")
	}

	// TTL истёк — промах (протухшая запись не отдаётся).
	now = now.Add(2 * time.Second)
	if _, hit := c.get(1, "http.", "24h", "", ""); hit {
		t.Fatalf("после истечения TTL должен быть промах")
	}
}

// TestAttrKeysCacheWindowIsolation — правка ревью UX Important #3: то же
// (projectID, prefix), но другое окно (period/start/end) — отдельная запись
// кеша, не хит. Без этого автокомплит с ДРУГИМ окном фильтра мог получить
// закешированный ответ от предыдущего окна (см. attrKeysCacheKey).
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

// TestAttrKeysCacheOverflowClearsAll закрепляет вытеснение при переполнении
// (см. maxAttrKeysCacheEntries): в отличие от KeyCache (ярусное вытеснение),
// здесь при достижении потолка карта очищается целиком — записи
// короткоживущие (TTL 60с) и дёшевы для пересчёта.
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

	// Запись поверх потолка вытесняет ВСЁ (включая только что положенные
	// записи 0..N-1), а не только освобождает место под одну новую.
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
