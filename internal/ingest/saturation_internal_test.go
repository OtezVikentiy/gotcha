package ingest

import (
	"strings"
	"testing"
)

func TestPipelineQueueSaturationEmpty(t *testing.T) {
	p := NewPipeline(nil, nil)
	if got := p.QueueSaturation(); got != 0 {
		t.Fatalf("QueueSaturation() = %v на пустой очереди, want 0", got)
	}
}

func TestPipelineQueueSaturationRowCap(t *testing.T) {
	p := NewPipeline(nil, nil)
	capacity := int(p.QueueCap())

	prev := p.QueueSaturation()
	for i := 0; i < capacity; i++ {
		p.Enqueue(1, 1, &ParsedEvent{EventID: "e"})
		got := p.QueueSaturation()
		if got < prev {
			t.Fatalf("QueueSaturation() = %v упала ниже предыдущего %v при заполнении очереди", got, prev)
		}
		prev = got
	}
	if got := p.Queued(); got != int64(capacity) {
		t.Fatalf("Queued() = %d, want %d (очередь должна была наполниться целиком)", got, capacity)
	}
	if got := p.QueueSaturation(); got < 1 {
		t.Fatalf("QueueSaturation() = %v при очереди на потолке по числу задач, want >= 1", got)
	}
}

func TestPipelineQueueSaturationByteCap(t *testing.T) {
	p := NewPipeline(nil, nil)
	ev := &ParsedEvent{EventID: "e", ContextsJSON: strings.Repeat("x", 200)}
	size := taskBytes(task{ev: ev})
	p.SetMaxQueueBytes(size)

	p.Enqueue(1, 1, ev)

	if got := p.QueuedBytes(); got != size {
		t.Fatalf("QueuedBytes() = %d, want %d (задача должна была зарезервировать ровно свой вес)", got, size)
	}
	if got := p.QueueSaturation(); got < 1 {
		t.Fatalf("QueueSaturation() = %v при очереди на потолке по байтам (счётный потолок почти пуст: 1/%d), want >= 1",
			got, p.QueueCap())
	}
}

func TestPipelineQueueSaturationPartial(t *testing.T) {
	p := NewPipeline(nil, nil)
	for i := 0; i < 3; i++ {
		p.Enqueue(1, 1, &ParsedEvent{EventID: "e"})
	}
	got := p.QueueSaturation()
	if got <= 0 || got >= 1 {
		t.Fatalf("QueueSaturation() = %v при частичном заполнении, want строго между 0 и 1", got)
	}
}

func TestQueueSaturationZeroDenominatorIsUnbounded(t *testing.T) {
	if got := queueSaturation(5, 0); got != 0 {
		t.Fatalf("queueSaturation(5, 0) = %v, want 0", got)
	}
	if got := queueSaturation(5, -1); got != 0 {
		t.Fatalf("queueSaturation(5, -1) = %v, want 0", got)
	}
}
