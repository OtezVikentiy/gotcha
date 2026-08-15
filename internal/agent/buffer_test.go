package agent

import "testing"

func TestBufferPushEvictsOldestByBatches(t *testing.T) {
	b := NewBuffer(2, 1000)
	b.Push([]byte("a"))
	b.Push([]byte("b"))
	b.Push([]byte("c")) // третий сверх maxBatches(2) — вытесняет "a"

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
	b.Push([]byte("aa")) // 2
	b.Push([]byte("bb")) // 4 суммарно
	b.Push([]byte("cc")) // 6 > 5 — вытесняет "aa", остаётся bb+cc=4

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
	b.Push([]byte("aaa"))        // 3, влезает
	b.Push([]byte("bbbbbbbbbb")) // 10 > maxBytes(5) — отбрасывается сам

	if got := b.Len(); got != 1 {
		t.Fatalf("Len() = %d, хочу 1 (переполнивший батч не должен опустошать буфер)", got)
	}
	oldest, ok := b.Oldest()
	if !ok || string(oldest) != "aaa" {
		t.Fatalf("Oldest() = %q, %v, хочу aaa, true", oldest, ok)
	}
}

func TestBufferOldestWithoutDropDoesNotRemove(t *testing.T) {
	b := NewBuffer(10, 100)
	b.Push([]byte("x"))

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
	b.Push([]byte("x"))
	b.Push([]byte("y"))
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
