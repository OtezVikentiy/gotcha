package event

import (
	"sync"
	"testing"
	"time"
)

// Follow-up (2026-08-12): при переполнении буфера писатель выбрасывает самое
// старое, и без per-org атрибуции потеря невидима per-org (тот же класс, что
// дропы очереди в arch P1-1, но слой буфера писателя). SetDropSink списывает
// выброшенные события их организациям; проверяем, что сток получает верные
// per-org счётчики и что событие без OrgID (0) не атрибутируется никому.
func TestBatcherAttributesDropsPerOrg(t *testing.T) {
	var mu sync.Mutex
	got := map[int64]int64{}
	c := &fakeConn{}
	b := NewBatcher(c)
	b.maxBuf = 4
	b.interval = time.Hour // не флашим по тику
	b.batchSize = 100      // не флашим по наполнению
	b.SetDropSink(func(orgID, n int64) {
		mu.Lock()
		got[orgID] += n
		mu.Unlock()
	})

	// Заполняем буфер (oldest→newest): org1, org0 (без атрибуции), org2, org2.
	for _, org := range []int64{1, 0, 2, 2} {
		b.Add(Event{ID: "x", OrgID: org})
	}
	// Три новых события org3 выбивают три самых старых: org1, затем org0
	// (не атрибутируется), затем org2.
	for i := 0; i < 3; i++ {
		b.Add(Event{ID: "new", OrgID: 3})
	}

	if d := b.Dropped(); d != 3 {
		t.Fatalf("Dropped() = %d, want 3", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[1] != 1 {
		t.Errorf("сток org1 = %d, want 1", got[1])
	}
	if got[2] != 1 {
		t.Errorf("сток org2 = %d, want 1", got[2])
	}
	if _, ok := got[0]; ok {
		t.Errorf("org0 (без OrgID) не должен атрибутироваться, а сток получил %d", got[0])
	}
	if got[3] != 0 {
		t.Errorf("org3 ничего не терял, а сток получил %d", got[3])
	}
}

// Без стока (SetDropSink не вызван) атрибуция — no-op: дропы просто считаются
// суммарно, как раньше. Гарантирует, что писатель без пайплайна (напр. в
// тестах) не паникует на nil-стоке.
func TestBatcherDropWithoutSinkIsNoop(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.maxBuf = 2
	b.interval = time.Hour
	b.batchSize = 100
	for i := 0; i < 5; i++ {
		b.Add(Event{ID: "x", OrgID: 7})
	}
	if d := b.Dropped(); d != 3 {
		t.Fatalf("Dropped() = %d, want 3", d)
	}
}
