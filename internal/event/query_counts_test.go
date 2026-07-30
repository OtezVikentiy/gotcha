package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestCountsSinceGroupsAllIssuesInOneQuery: одна выборка отдаёт счётчики всех
// групп сразу. Раньше spike-детектор считал их по одной — тик стоил столько
// round-trip'ов в ClickHouse, сколько в проекте активных групп, а их число
// задаёт отправитель событий.
func TestCountsSinceGroupsAllIssuesInOneQuery(t *testing.T) {
	ch := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := event.NewQuery(ch)

	const projectID = 991
	b := event.NewBatcher(ch)
	go b.Run()
	now := time.Now().UTC()
	// Группа 1 — 5 событий, группа 2 — 2 события, группа 3 — 4, но старые.
	for i := 0; i < 5; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: 1,
			Timestamp: now.Add(-time.Duration(i) * time.Minute), Level: "error", Message: "boom"})
	}
	for i := 0; i < 2; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: 2,
			Timestamp: now.Add(-time.Duration(i) * time.Minute), Level: "error", Message: "boom"})
	}
	for i := 0; i < 4; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: 3,
			Timestamp: now.Add(-3 * time.Hour), Level: "error", Message: "old"})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("batcher close: %v", err)
	}

	counts, err := q.CountsSince(ctx, projectID, now.Add(-30*time.Minute), 3)
	if err != nil {
		t.Fatalf("CountsSince: %v", err)
	}

	if got := counts[1]; got != 5 {
		t.Errorf("группа 1: %d, want 5", got)
	}
	if _, ok := counts[2]; ok {
		t.Errorf("группа 2 попала в ответ, хотя не дотянула до порога — порог должен " +
			"применяться в запросе, а не после того, как данные проехали по сети")
	}
	if _, ok := counts[3]; ok {
		t.Errorf("группа 3 попала в ответ, хотя её события старше окна")
	}
}
