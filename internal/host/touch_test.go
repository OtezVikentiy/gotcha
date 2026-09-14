package host

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func entries(names ...string) []TouchEntry {
	out := make([]TouchEntry, len(names))
	for i, n := range names {
		out[i] = TouchEntry{Name: n}
	}
	return out
}

func TestToucherThrottles(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	tc := NewToucher(nil, time.Hour, 10)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		for _, e := range entries {
			calls[e.Name]++
		}
		mu.Unlock()
		return nil
	}
	tc.Touch(context.Background(), 1, entries("a"))
	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 1 {
		t.Fatalf("upserts = %d, want 1", calls["a"])
	}
}

func TestToucherForgetAllowsImmediateRetouch(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	tc := NewToucher(nil, time.Hour, 10)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		for _, e := range entries {
			calls[e.Name]++
		}
		mu.Unlock()
		return nil
	}

	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()
	tc.Forget(1, "a")
	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 2 {
		t.Fatalf("upserts = %d, want 2 (Forget должен снять троттлинг)", calls["a"])
	}
}

func TestToucherRetriesAfterFailedUpsert(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tc := NewToucher(nil, time.Hour, 10)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("pg is down")
	}

	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()
	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("upserts = %d, want 2 (после ошибки ключ обязан выйти из троттлинга)", calls)
	}
	if got := tc.UpsertFailures(); got != 2 {
		t.Errorf("UpsertFailures = %d, want 2", got)
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, ok := tc.seen[touchKey{projectID: 1, name: "a"}]; ok {
		t.Error("ключ остался помеченным seen после провала upsert")
	}
}

func TestToucherSkipsPathTraversalNames(t *testing.T) {
	var mu sync.Mutex
	var got []string
	tc := NewToucher(nil, time.Hour, 10)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		for _, e := range entries {
			got = append(got, e.Name)
		}
		mu.Unlock()
		return nil
	}

	tc.Touch(context.Background(), 1, entries(".", "..", "", "web-01"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "web-01" {
		t.Fatalf("зарегистрированы %q, want только [web-01]", got)
	}
}

func TestToucherThrottleIgnoresVersionChange(t *testing.T) {
	var mu sync.Mutex
	var got []TouchEntry
	tc := NewToucher(nil, time.Hour, 10)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		got = append(got, entries...)
		mu.Unlock()
		return nil
	}

	tc.Touch(context.Background(), 1, []TouchEntry{{Name: "a", AgentVersion: "0.6.0"}})
	tc.Touch(context.Background(), 1, []TouchEntry{{Name: "a", AgentVersion: "0.6.1"}})
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].AgentVersion != "0.6.0" {
		t.Fatalf("upserts = %+v, want ровно один с версией 0.6.0 (смена версии не должна пробивать троттлинг)", got)
	}
}

func TestToucherEvictsOldest(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	tc := NewToucher(nil, time.Hour, 2)
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		for _, e := range entries {
			calls[e.Name]++
		}
		mu.Unlock()
		return nil
	}

	tc.Touch(context.Background(), 1, entries("a"))
	time.Sleep(2 * time.Millisecond) // порядок вытеснения зависит от времени записи
	tc.Touch(context.Background(), 1, entries("b"))
	tc.wait()

	tc.mu.Lock()
	size := len(tc.seen)
	tc.mu.Unlock()
	if size != 2 {
		t.Fatalf("len(seen) = %d, want 2 после двух ключей при maxEntries=2", size)
	}

	time.Sleep(2 * time.Millisecond)
	tc.Touch(context.Background(), 1, entries("c"))
	tc.wait()

	tc.mu.Lock()
	size = len(tc.seen)
	_, hasA := tc.seen[touchKey{projectID: 1, name: "a"}]
	_, hasC := tc.seen[touchKey{projectID: 1, name: "c"}]
	tc.mu.Unlock()
	if size != 2 {
		t.Fatalf("len(seen) = %d, want 2 (карта не должна расти сверх maxEntries)", size)
	}
	if hasA {
		t.Fatal("запись \"a\" должна быть вытеснена как самая старая")
	}
	if !hasC {
		t.Fatal("новая запись \"c\" должна попасть в карту")
	}

	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 2 {
		t.Fatalf("upserts[a] = %d, want 2 (вытеснение должно снять троттлинг)", calls["a"])
	}
}

// Порядок вставки и порядок активности могут расходиться: "a" вставлена раньше "b", но её
// тронули позже — вытеснить обязана "b" как давно не трогавшуюся, а не "a" как вставленную первой.
func TestToucherEvictsLeastRecentlyTouchedNotOldestInserted(t *testing.T) {
	tc := NewToucher(nil, 2*time.Millisecond, 2)

	tc.Touch(context.Background(), 1, entries("a"))
	time.Sleep(5 * time.Millisecond)
	tc.Touch(context.Background(), 1, entries("b"))
	time.Sleep(5 * time.Millisecond)
	tc.Touch(context.Background(), 1, entries("a")) // every истёк — обязано сдвинуть "a" в конец очереди
	time.Sleep(time.Millisecond)
	tc.Touch(context.Background(), 1, entries("c"))
	tc.wait()

	tc.mu.Lock()
	_, hasA := tc.seen[touchKey{projectID: 1, name: "a"}]
	_, hasB := tc.seen[touchKey{projectID: 1, name: "b"}]
	_, hasC := tc.seen[touchKey{projectID: 1, name: "c"}]
	tc.mu.Unlock()

	if hasB {
		t.Error("\"b\" обязана быть вытеснена: с момента вставки её не трогали, она самая давно не тронутая")
	}
	if !hasA {
		t.Error("\"a\" не должна быть вытеснена: её тронули после \"b\", вытеснение по порядку ВСТАВКИ перепутало бы её с \"b\"")
	}
	if !hasC {
		t.Error("\"c\" — новая запись, обязана попасть в карту")
	}
}
