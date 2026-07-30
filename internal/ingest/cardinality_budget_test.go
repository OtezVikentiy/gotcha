package ingest

import (
	"fmt"
	"testing"
	"time"
)

// TestFieldCountIsCapped: имена полей задаёт отправитель, поэтому «поле на
// событие» обходило ограничитель целиком — потолок стоял только на значениях
// внутри поля, а карта полей росла без границы.
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
	// Поле сверх потолка обязано схлопываться, а не проходить как есть.
	if got := g.Value(1, "field-99999", "value"); got != CardinalityOverflow {
		t.Errorf("значение поля сверх потолка = %q, want %q", got, CardinalityOverflow)
	}
}

// TestTrackedValuesRespectBudget: общий бюджет запомненных значений — та самая
// граница, которой не было. Произведение потолков (проекты × поля × значения)
// давало миллиарды строк, то есть «границу» размером в сотни гигабайт.
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

// TestExpiredProjectsFreeBudget: набор проекта с истёкшим окном освобождает
// бюджет. Иначе инстанс, переживший всплеск, навсегда остаётся с исчерпанным
// бюджетом и перестаёт различать значения даже у здоровых проектов.
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

	// Окно проекта 1 истекло — его набор больше не нужен.
	now = now.Add(2 * time.Hour)
	if got := g.Value(2, "tag", "fresh"); got != "fresh" {
		t.Errorf("значение нового проекта схлопнуто (%q) при освободившемся бюджете", got)
	}
}
