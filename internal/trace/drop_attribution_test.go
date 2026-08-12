package trace

import (
	"sync"
	"testing"
	"time"
)

// Follow-up (2026-08-12): при переполнении txBuf SpanWriter выбрасывает самые
// старые транзакции; SetDropSink списывает их организациям (org_usage.
// dropped_transactions). Проверяем per-org счётчики и пропуск транзакции без
// OrgID (0). Дропы spanBuf в счётчик не идут — транзакция, а не спан, есть
// квота-единица; здесь spanBuf не переполняем (maxSpanBuf по умолчанию велик).
func TestSpanWriterAttributesTxDropsPerOrg(t *testing.T) {
	var mu sync.Mutex
	got := map[int64]int64{}
	w := NewSpanWriter(&fakeCHConn{})
	w.maxBuf = 3           // маленький потолок txBuf
	w.interval = time.Hour // не флашим по тику
	w.batchSize = 100      // не флашим по наполнению
	w.SetDropSink(func(orgID, n int64) {
		mu.Lock()
		got[orgID] += n
		mu.Unlock()
	})

	// Заполняем txBuf (oldest→newest): org1, org0 (без атрибуции), org2.
	for _, org := range []int64{1, 0, 2} {
		w.Add(org, 1, sampleTx(0))
	}
	// Три новых транзакции org3 выбивают три самых старых: org1, org0
	// (не атрибутируется), org2.
	for i := 0; i < 3; i++ {
		w.Add(3, 1, sampleTx(0))
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
