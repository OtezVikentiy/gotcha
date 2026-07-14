package ingest

import "testing"

// TestEnqueueAfterCloseDoesNotPanic покрывает гонку main.go: drain() закрывает
// очередь (Close), пока ещё не завершившиеся обработчики могут звать Enqueue.
// До фикса это паниковало (send on closed channel).
func TestEnqueueAfterCloseDoesNotPanic(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.Start()
	p.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enqueue after Close panicked: %v", r)
		}
	}()
	p.Enqueue(1, &ParsedEvent{EventID: "x"})
}

// TestDoubleCloseDoesNotPanic — Close должен быть идемпотентным (закрытие
// уже закрытого канала паникует).
func TestDoubleCloseDoesNotPanic(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.Start()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("double Close panicked: %v", r)
		}
	}()
	p.Close()
	p.Close()
}
