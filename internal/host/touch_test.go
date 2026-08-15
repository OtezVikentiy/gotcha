package host

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// entries — короткая сборка []TouchEntry из одних имён (без версии агента)
// для тестов, которым версия не важна.
func entries(names ...string) []TouchEntry {
	out := make([]TouchEntry, len(names))
	for i, n := range names {
		out[i] = TouchEntry{Name: n}
	}
	return out
}

// TestToucherThrottles — второй Touch того же имени в пределах every
// подавляется, upsert вызывается один раз.
func TestToucherThrottles(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	tc := NewToucher(nil, time.Hour, 10) // store=nil: подменяем upsert
	tc.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		mu.Lock()
		for _, e := range entries {
			calls[e.Name]++
		}
		mu.Unlock()
		return nil
	}
	tc.Touch(context.Background(), 1, entries("a"))
	tc.Touch(context.Background(), 1, entries("a")) // в пределах every — подавлен
	tc.wait()                                       // дождаться горутин

	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 1 {
		t.Fatalf("upserts = %d, want 1", calls["a"])
	}
}

// TestToucherForgetAllowsImmediateRetouch — Forget снимает троттлинг для
// конкретного (project, host): следующий Touch проходит немедленно, будто
// его ещё не было.
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

// TestToucherRetriesAfterFailedUpsert — провалившийся upsert снимает пометку
// троттлинга: следующий батч пробует то же имя снова, не дожидаясь every.
// Иначе недоступность PostgreSQL «съедала» бы регистрацию — last_seen хоста не
// обновлялся бы ещё минуту после КАЖДОЙ неудачной попытки, и оценщик открыл бы
// silent-инцидент по живой машине.
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
	tc.Touch(context.Background(), 1, entries("a")) // every=час, но прошлый upsert провалился
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

// TestToucherSkipsPathTraversalNames — "." и ".." не регистрируются: имя хоста
// едет в путь карточки, и такие имена невозможно ни открыть, ни удалить.
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

// TestToucherThrottleIgnoresVersionChange — троттлинг ключуется только по
// имени (touchKey), не по (имя, версия): второй Touch того же имени с ДРУГОЙ
// версией агента в пределах every всё равно подавляется — упавшая версия
// долетит со следующим тиком не позже every, форсировать внеочередной upsert
// ради неё не оправдано (осознанно, спека §3.2).
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
	tc.Touch(context.Background(), 1, []TouchEntry{{Name: "a", AgentVersion: "0.6.1"}}) // в пределах every — подавлен, версия не долетит
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].AgentVersion != "0.6.0" {
		t.Fatalf("upserts = %+v, want ровно один с версией 0.6.0 (смена версии не должна пробивать троттлинг)", got)
	}
}

// TestToucherEvictsOldest — при maxEntries=2 третий уникальный ключ
// вытесняет самую старую запись, карта не растёт без границы.
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
	tc.Touch(context.Background(), 1, entries("c")) // должен вытеснить "a" — самую старую запись
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

	// Раз "a" вытеснена — троттлинг для неё снят, повторный Touch снова
	// проходит и увеличивает счётчик upsert'ов.
	tc.Touch(context.Background(), 1, entries("a"))
	tc.wait()

	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 2 {
		t.Fatalf("upserts[a] = %d, want 2 (вытеснение должно снять троттлинг)", calls["a"])
	}
}
