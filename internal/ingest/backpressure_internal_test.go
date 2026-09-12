package ingest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

type stepSatEventSink struct {
	freeAfter int
	checks    atomic.Int64
	added     atomic.Int64
}

func (s *stepSatEventSink) Saturation() float64 {
	n := s.checks.Add(1)
	if s.freeAfter > 0 && int(n) > s.freeAfter {
		return 0.5
	}
	return 1.0
}

func (s *stepSatEventSink) Add(event.Event) { s.added.Add(1) }

type stepSatSpanSink struct {
	freeAfter int
	checks    atomic.Int64
	added     atomic.Int64
}

func (s *stepSatSpanSink) Saturation() float64 {
	n := s.checks.Add(1)
	if s.freeAfter > 0 && int(n) > s.freeAfter {
		return 0.5
	}
	return 1.0
}

func (s *stepSatSpanSink) Add(_, _ int64, _ trace.Transaction) { s.added.Add(1) }

func TestBackpressureWaitsThenWritesEvent(t *testing.T) {
	sink := &stepSatEventSink{freeAfter: 3}
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = sink
	p.testBackpressureBudget = time.Second
	p.testBackpressurePoll = 2 * time.Millisecond
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && sink.added.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := sink.added.Load(); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1: событие потеряно вместо ожидания освобождения", got)
	}
	if got := p.BackpressureWaits(); got != 1 {
		t.Fatalf("BackpressureWaits() = %d, want 1: воркер обязан был подождать насыщенный батчер", got)
	}
	if got := sink.checks.Load(); got <= int64(sink.freeAfter) {
		t.Fatalf("Saturation() calls = %d, want > %d: ожидание не опрашивало насыщенность повторно", got, sink.freeAfter)
	}
	if got := p.BackpressureWaitSeconds(); got <= 0 || got > 1.5 {
		t.Errorf("BackpressureWaitSeconds() = %v, want в (0, 1.5]: величина "+
			"обязана быть в секундах, не в наносекундах (мутация: метрика "+
			"возвращает наносекунды под именем seconds?)", got)
	}
}

func TestBackpressureWaitsThenWritesTransaction(t *testing.T) {
	sink := &stepSatSpanSink{freeAfter: 3}
	p := NewPipeline(nil, nil)
	p.Spans = sink
	p.testBackpressureBudget = time.Second
	p.testBackpressurePoll = 2 * time.Millisecond
	p.Start()

	p.EnqueueTransaction(1, 1, nPlusOneTx())

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && sink.added.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := sink.added.Load(); got != 1 {
		t.Fatalf("Spans.Add calls = %d, want 1: транзакция потеряна вместо ожидания освобождения", got)
	}
	if got := p.BackpressureWaits(); got != 1 {
		t.Fatalf("BackpressureWaits() = %d, want 1: воркер обязан был подождать насыщенный SpanSink", got)
	}
	if got := sink.checks.Load(); got <= int64(sink.freeAfter) {
		t.Fatalf("Saturation() calls = %d, want > %d: ожидание не опрашивало насыщенность повторно", got, sink.freeAfter)
	}
}

func TestBackpressureBudgetExpiresEventStillWrites(t *testing.T) {
	sink := &stepSatEventSink{freeAfter: 0} // никогда не освобождается
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = sink
	p.testBackpressureBudget = 40 * time.Millisecond
	p.testBackpressurePoll = 5 * time.Millisecond
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && sink.added.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := sink.added.Load(); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1 в пределах потолка теста: "+
			"бюджет ожидания не ограничил воркер (мутация: бюджет бесконечен?)", got)
	}
	if got := p.BackpressureWaits(); got != 1 {
		t.Errorf("BackpressureWaits() = %d, want 1", got)
	}

	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBackpressureNoWaitWhenNotSaturatedEvent(t *testing.T) {
	sink := &satEventSink{sat: 0.5}
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = sink
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := sink.count(); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1", got)
	}
	if got := p.BackpressureWaits(); got != 0 {
		t.Errorf("BackpressureWaits() = %d, want 0: ненасыщенный приёмник не должен вызывать ожидание", got)
	}
}

func TestBackpressureNoWaitWithoutSaturationSourceEvent(t *testing.T) {
	fb := &fakeBatcher{}
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = fb
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := len(fb.evs); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1", got)
	}
	if got := p.BackpressureWaits(); got != 0 {
		t.Errorf("BackpressureWaits() = %d, want 0: приёмник без Saturation() не должен вызывать ожидание", got)
	}
}

func TestBackpressureNoWaitWithoutSaturationSourceTransaction(t *testing.T) {
	spans := &fakeSpanSink{}
	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Start()

	p.EnqueueTransaction(1, 1, nPlusOneTx())
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := spans.count(); got != 1 {
		t.Fatalf("Spans.Add calls = %d, want 1", got)
	}
	if got := p.BackpressureWaits(); got != 0 {
		t.Errorf("BackpressureWaits() = %d, want 0: приёмник без Saturation() не должен вызывать ожидание", got)
	}
}

func TestBackpressureCloseInterruptsWaitEvent(t *testing.T) {
	sink := &stepSatEventSink{freeAfter: 0} // никогда не освобождается
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = sink
	p.testBackpressureBudget = 2 * time.Second
	p.testBackpressurePoll = 5 * time.Millisecond
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})

	start := time.Now()
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close занял %v, want < 500ms: ожидание не прервалось остановкой "+
			"(мутация: убрано прерывание по stopping?)", elapsed)
	}

	if got := sink.added.Load(); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1: задача не должна теряться на остановке", got)
	}
}

func TestBackpressureCloseInterruptsWaitTransaction(t *testing.T) {
	sink := &stepSatSpanSink{freeAfter: 0} // никогда не освобождается
	p := NewPipeline(nil, nil)
	p.Spans = sink
	p.testBackpressureBudget = 2 * time.Second
	p.testBackpressurePoll = 5 * time.Millisecond
	p.Start()

	p.EnqueueTransaction(1, 1, nPlusOneTx())

	start := time.Now()
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close занял %v, want < 500ms: ожидание не прервалось остановкой "+
			"(мутация: убрано прерывание по stopping?)", elapsed)
	}

	if got := sink.added.Load(); got != 1 {
		t.Fatalf("Spans.Add calls = %d, want 1: транзакция не должна теряться на остановке", got)
	}
}

func TestBackpressureCloseInterruptsWaitOnLiteralPipeline(t *testing.T) {
	sink := &stepSatEventSink{freeAfter: 0} // никогда не освобождается
	p := &Pipeline{
		issues:  &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}},
		queue:   make(chan task, 10),
		workers: 1,
		dropped: newDropCounters(),
		dropAgg: make(map[dropAggKey]int64),
		batcher: sink,
	}
	p.testBackpressureBudget = 2 * time.Second
	p.testBackpressurePoll = 5 * time.Millisecond
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && sink.checks.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close на литеральном Pipeline занял %v, want < 500ms: "+
			"остановка не прерывает ожидание у Pipeline, собранного в обход "+
			"NewPipeline (регрессия к nil-каналу stopWait?)", elapsed)
	}

	if got := sink.added.Load(); got != 1 {
		t.Fatalf("batcher.Add calls = %d, want 1: задача не должна теряться на остановке", got)
	}
}
