package metric

import (
	"sync"
	"testing"
	"time"
)

// Точка без project_id (0) не атрибутируется в сток; переполнение буфера —
// не единственная причина дропа, но единственная, которую видит trimLocked.
func TestWriterAttributesDropsPerProject(t *testing.T) {
	var mu sync.Mutex
	got := map[uint64]int64{}
	w := NewWriter(&fakeCHConn{})
	w.maxBuf = 3           // маленький потолок буфера
	w.interval = time.Hour // не флашим по тику
	w.batchSize = 100      // не флашим по наполнению
	w.SetDropSink(func(projectID uint64, n int64) {
		mu.Lock()
		got[projectID] += n
		mu.Unlock()
	})

	now := time.Now().UTC()
	// Заполняем buf (oldest→newest): project1, project0 (без атрибуции), project2.
	for _, p := range []int64{1, 0, 2} {
		w.Add(p, MetricPoint{Name: "m", Type: TypeGauge, TS: now, Value: 1})
	}
	// Три новые точки project3 выбивают три самых старых: project1, project0
	// (не атрибутируется), project2.
	for i := 0; i < 3; i++ {
		w.Add(3, MetricPoint{Name: "m", Type: TypeGauge, TS: now, Value: 1})
	}

	mu.Lock()
	defer mu.Unlock()
	if got[1] != 1 {
		t.Errorf("сток project1 = %d, want 1", got[1])
	}
	if got[2] != 1 {
		t.Errorf("сток project2 = %d, want 1", got[2])
	}
	if _, ok := got[0]; ok {
		t.Errorf("project0 (без project_id) не должен атрибутироваться, а сток получил %d", got[0])
	}
	if got[3] != 0 {
		t.Errorf("project3 ничего не терял, а сток получил %d", got[3])
	}
}
