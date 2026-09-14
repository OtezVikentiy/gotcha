package agent

import "testing"

func TestBufferPushEvictsOldestByBatches(t *testing.T) {
	b := NewBuffer(2, 1000)
	b.Push([]byte("a"), 1)
	b.Push([]byte("b"), 1)
	b.Push([]byte("c"), 1) // третий сверх maxBatches(2) — вытесняет "a"

	if got := b.Len(); got != 2 {
		t.Fatalf("Len() = %d, хочу 2", got)
	}
	oldest, ok := b.Oldest()
	if !ok || string(oldest) != "b" {
		t.Fatalf("Oldest() = %q, %v, хочу b, true", oldest, ok)
	}
}

func TestBufferPushEvictsOldestByBytes(t *testing.T) {
	b := NewBuffer(100, 5)
	b.Push([]byte("aa"), 1) // 2
	b.Push([]byte("bb"), 1) // 4 суммарно
	b.Push([]byte("cc"), 1) // 6 > 5 — вытесняет "aa", остаётся bb+cc=4

	if got := b.Len(); got != 2 {
		t.Fatalf("Len() = %d, хочу 2", got)
	}
	oldest, ok := b.Oldest()
	if !ok || string(oldest) != "bb" {
		t.Fatalf("Oldest() = %q, %v, хочу bb, true", oldest, ok)
	}
}

func TestBufferPushDropsOversizedBatchAlone(t *testing.T) {
	b := NewBuffer(100, 5)
	b.Push([]byte("aaa"), 1)         // 3, влезает
	b.Push([]byte("bbbbbbbbbb"), 42) // 10 > maxBytes(5) — отбрасывается сам

	if got := b.Len(); got != 1 {
		t.Fatalf("Len() = %d, хочу 1 (переполнивший батч не должен опустошать буфер)", got)
	}
	oldest, ok := b.Oldest()
	if !ok || string(oldest) != "aaa" {
		t.Fatalf("Oldest() = %q, %v, хочу aaa, true", oldest, ok)
	}
}

// Точки различны у каждого батча — доказывает, что счётчик считает точки
// вытесненного батча, а не число вытесненных батчей (совпало бы на 1).
func TestBufferTakeDroppedCountsEvictionInPoints(t *testing.T) {
	b := NewBuffer(2, 1000)
	b.Push([]byte("a"), 5)
	b.Push([]byte("b"), 7)
	b.Push([]byte("c"), 9) // вытесняет "a" (5 точек)

	oversized, evicted := b.TakeDropped()
	if oversized != 0 {
		t.Errorf("oversized = %d, хочу 0", oversized)
	}
	if evicted != 5 {
		t.Fatalf("evicted = %d, хочу 5 (точки вытесненного \"a\", не число батчей)", evicted)
	}

	oversized, evicted = b.TakeDropped()
	if oversized != 0 || evicted != 0 {
		t.Fatalf("TakeDropped() повторно = %d, %d, хочу 0, 0 (счётчик сброшен)", oversized, evicted)
	}
}

// 42 — точки самого переполнившего батча, а не 1 (число батчей): единица
// счётчика обязана быть точками.
func TestBufferTakeDroppedCountsOversizedInPoints(t *testing.T) {
	b := NewBuffer(100, 5)
	b.Push([]byte("aaa"), 1)         // влезает
	b.Push([]byte("bbbbbbbbbb"), 42) // 10 > maxBytes(5) — отброшен сам

	oversized, evicted := b.TakeDropped()
	if oversized != 42 {
		t.Errorf("oversized = %d, хочу 42", oversized)
	}
	if evicted != 0 {
		t.Errorf("evicted = %d, хочу 0", evicted)
	}
}

func TestBufferDropOldestDoesNotCountAsLoss(t *testing.T) {
	b := NewBuffer(10, 100)
	b.Push([]byte("x"), 3)
	b.Push([]byte("y"), 4)
	b.DropOldest() // успешная доставка снимает батч — это не потеря

	oversized, evicted := b.TakeDropped()
	if oversized != 0 || evicted != 0 {
		t.Fatalf("TakeDropped() после DropOldest успешной доставки = %d, %d, хочу 0, 0", oversized, evicted)
	}
}

func TestBufferOldestWithoutDropDoesNotRemove(t *testing.T) {
	b := NewBuffer(10, 100)
	b.Push([]byte("x"), 1)

	first, ok := b.Oldest()
	if !ok || string(first) != "x" {
		t.Fatalf("Oldest() первый вызов = %q, %v", first, ok)
	}
	second, ok := b.Oldest()
	if !ok || string(second) != "x" {
		t.Fatalf("Oldest() повторный вызов = %q, %v, хочу тот же элемент без удаления", second, ok)
	}
	if got := b.Len(); got != 1 {
		t.Fatalf("Len() = %d, хочу 1 — Oldest не должен удалять", got)
	}
}

func TestBufferDropOldest(t *testing.T) {
	b := NewBuffer(10, 100)
	b.Push([]byte("x"), 1)
	b.Push([]byte("y"), 1)
	b.DropOldest()

	if got := b.Len(); got != 1 {
		t.Fatalf("Len() = %d, хочу 1", got)
	}
	oldest, ok := b.Oldest()
	if !ok || string(oldest) != "y" {
		t.Fatalf("Oldest() = %q, %v, хочу y, true", oldest, ok)
	}
}

func TestBufferOldestEmpty(t *testing.T) {
	b := NewBuffer(10, 100)
	if _, ok := b.Oldest(); ok {
		t.Fatal("Oldest() на пустом буфере должен вернуть false")
	}
}

func TestBufferDropOldestEmptyNoop(t *testing.T) {
	b := NewBuffer(10, 100)
	b.DropOldest() // не должно паниковать на пустом буфере
	if got := b.Len(); got != 0 {
		t.Fatalf("Len() = %d, хочу 0", got)
	}
}
