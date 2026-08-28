package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

// TestCardinalityNoticesNilGuard — h.Cardinality==nil (веб-узел развёрнут
// отдельно от ingest, лимитер живёт в памяти другого процесса): предупреждений
// нет, а не паника на разыменовании.
func TestCardinalityNoticesNilGuard(t *testing.T) {
	h := &Handler{}
	if got := h.cardinalityNotices(1); got != nil {
		t.Errorf("cardinalityNotices() = %+v без Cardinality, want nil", got)
	}
}

// TestCardinalityNoticesEmptyReport — гард есть, но проект ни разу не упирался
// в потолок кардинальности: пусто, а не срез с нулевыми записями.
func TestCardinalityNoticesEmptyReport(t *testing.T) {
	h := &Handler{Cardinality: ingest.NewCardinalityGuard(10, time.Hour)}
	if got := h.cardinalityNotices(1); got != nil {
		t.Errorf("cardinalityNotices() = %+v без переполнений, want nil", got)
	}
}

// TestCardinalityNoticesWithReport — проект упёрся в потолок по имени
// транзакции: уведомление обязано нести человекочитаемое имя поля
// (ingest.FieldLabel), потолок, число схлопнутых значений и примеры — иначе
// это будет необъяснимый "<cardinality-limit>" в списке.
func TestCardinalityNoticesWithReport(t *testing.T) {
	g := ingest.NewCardinalityGuard(2, time.Hour)
	const projectID = 42
	g.Value(projectID, ingest.FieldTransaction, "GET /orders")
	g.Value(projectID, ingest.FieldTransaction, "POST /orders")
	for i := 0; i < 5; i++ {
		g.Value(projectID, ingest.FieldTransaction, "GET /users/"+string(rune('a'+i)))
	}
	h := &Handler{Cardinality: g}

	notices := h.cardinalityNotices(projectID)
	if len(notices) != 1 {
		t.Fatalf("notices = %+v, want 1 запись", notices)
	}
	n := notices[0]
	if n.Field != ingest.FieldLabel(ingest.FieldTransaction) {
		t.Errorf("Field = %q, want %q", n.Field, ingest.FieldLabel(ingest.FieldTransaction))
	}
	if n.Limit != 2 {
		t.Errorf("Limit = %d, want 2", n.Limit)
	}
	if n.Collapsed != 5 {
		t.Errorf("Collapsed = %d, want 5", n.Collapsed)
	}
	if len(n.Samples) == 0 {
		t.Errorf("Samples пусты, want хотя бы один пример схлопнутого значения")
	}

	// Другой проект не задет тем же гардом.
	if got := h.cardinalityNotices(projectID + 1); got != nil {
		t.Errorf("cardinalityNotices(другой проект) = %+v, want nil", got)
	}
}
