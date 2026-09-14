package escalation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// projectStuckMaint виснет до истечения бюджета тика и пишет project_id — так
// видно, какой инцидент реально дошёл до проверки maintenance за тик.
type projectStuckMaint struct {
	mu      sync.Mutex
	touched []int64
}

func (m *projectStuckMaint) InMaintenance(ctx context.Context, projectID int64, _ time.Time) (bool, error) {
	m.mu.Lock()
	m.touched = append(m.touched, projectID)
	m.mu.Unlock()
	<-ctx.Done()
	return false, ctx.Err()
}

func (m *projectStuckMaint) touchedCopy() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.touched...)
}

// Без ротации бюджет тика (пол 10с) всегда обрывается на самом старом инциденте.
func TestSchedulerRotatesPendingAcrossTicks(t *testing.T) {
	src := newFakeSource("metric")
	projectIDs := []int64{101, 102, 103}
	for i, pid := range projectIDs {
		src.add(escalation.PendingIncident{
			ID: int64(i + 1), ProjectID: pid, Severity: escalation.SeverityWarning,
			EscalationLevel: 0, StartedAt: time.Now(),
		})
	}

	maint := &projectStuckMaint{}
	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: &fakeNotifier{}}},
		Maint:    maint,
		Interval: time.Second,
		Now:      time.Now,
	}

	tickAndFirstTouched := func(tickNo int) int64 {
		before := len(maint.touchedCopy())
		sched.Tick(context.Background())
		got := maint.touchedCopy()[before:]
		if len(got) == 0 {
			t.Fatalf("тик %d: maintenance ни разу не проверен — тест не проверяет то, что должен", tickNo)
		}
		first := got[0]
		for _, pid := range got {
			if pid != first {
				t.Fatalf("тик %d: за один тик задет не один инцидент: %v", tickNo, got)
			}
		}
		return first
	}

	// За 4 тика курсор обязан пройти все три инцидента по кругу и вернуться к первому.
	want := []int64{projectIDs[0], projectIDs[1], projectIDs[2], projectIDs[0]}
	for i, w := range want {
		got := tickAndFirstTouched(i + 1)
		if got != w {
			t.Fatalf("тик %d: обработан инцидент проекта %d, want %d (порядок %v)", i+1, got, w, projectIDs)
		}
		if skipped := sched.LastTickSkippedIncidents(); skipped != 2 {
			t.Errorf("тик %d: LastTickSkippedIncidents() = %d, want 2 (два инцидента из трёх не влезли в бюджет)", i+1, skipped)
		}
	}
}
