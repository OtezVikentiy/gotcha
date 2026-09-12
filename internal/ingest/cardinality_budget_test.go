package ingest

import (
	"fmt"
	"testing"
	"time"
)

func TestFieldCountIsCapped(t *testing.T) {
	g := NewCardinalityGuard(10, time.Hour)

	for i := 0; i < maxCardinalityFields+50; i++ {
		g.Value(1, fmt.Sprintf("field-%d", i), "value")
	}

	g.mu.Lock()
	fields := len(g.projects[1].fields)
	g.mu.Unlock()

	if fields > maxCardinalityFields {
		t.Errorf("отслеживается %d полей при потолке %d: имя поля задаёт отправитель, "+
			"и без потолка карта растёт без границы", fields, maxCardinalityFields)
	}
	if got := g.Value(1, "field-99999", "value"); got != CardinalityOverflow {
		t.Errorf("значение поля сверх потолка = %q, want %q", got, CardinalityOverflow)
	}
}

func TestTrackedValuesRespectBudget(t *testing.T) {
	g := NewCardinalityGuard(1000, time.Hour)
	g.maxTracked = 100

	for p := 0; p < 5; p++ {
		for i := 0; i < 100; i++ {
			g.Value(int64(p+1), "tag", fmt.Sprintf("v-%d-%d", p, i))
		}
	}

	if got := g.TrackedValues(); got > 100 {
		t.Errorf("запомнено %d значений при бюджете 100 — бюджет не соблюдается", got)
	}
	if got := g.TrackedValues(); got == 0 {
		t.Error("не запомнено ни одного значения — ограничитель перестал работать вовсе")
	}
}

func TestExpiredProjectsFreeBudget(t *testing.T) {
	now := time.Now()
	g := NewCardinalityGuard(1000, time.Hour)
	g.maxTracked = 50
	g.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		g.Value(1, "tag", fmt.Sprintf("v%d", i))
	}
	if got := g.TrackedValues(); got != 50 {
		t.Fatalf("запомнено %d, want 50", got)
	}

	now = now.Add(2 * time.Hour)
	if got := g.Value(2, "tag", "fresh"); got != "fresh" {
		t.Errorf("значение нового проекта схлопнуто (%q) при освободившемся бюджете", got)
	}
}
