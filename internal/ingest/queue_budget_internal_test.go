package ingest

import (
	"strings"
	"testing"
)

// TestQueueByteBudgetDropsOversizedFlood — очередь ограничена не только по
// числу задач, но и по объёму.
//
// Счётный потолок сам по себе ничего не гарантировал: событие несёт до четырёх
// сырых JSON-блоков по 256 КиБ (contexts, breadcrumbs, request, stacktrace) —
// до мегабайта на задачу, — а очередь держит тысячу задач. Гигабайт
// резидентной памяти на пути приёма, куда пишет кто угодно с публичным ключом.
// Все пять писателей получили байтовый бюджет ровно по этой причине; очередь
// была единственным буфером без него.
func TestQueueByteBudgetDropsOversizedFlood(t *testing.T) {
	p := NewPipeline(nil, nil)
	// Воркеры не запускаем: очередь должна наполниться и упереться в потолок.
	p.SetMaxQueueBytes(1 << 20) // 1 МиБ

	big := strings.Repeat("x", 256<<10)
	accepted := 0
	for i := 0; i < 100; i++ {
		before := p.Queued()
		p.Enqueue(1, &ParsedEvent{EventID: "e", ContextsJSON: big})
		if p.Queued() > before {
			accepted++
		}
	}

	if accepted == 0 {
		t.Fatal("ни одно событие не принято — бюджет не пропускает вообще ничего")
	}
	// 1 МиБ бюджета при 256 КиБ на событие — порядка четырёх штук; счётный
	// лимит (1000) тут не при чём.
	if accepted > 8 {
		t.Fatalf("принято %d событий по 256 КиБ при бюджете 1 МиБ — потолок не работает", accepted)
	}
	if got := p.QueuedBytes(); got > 1<<20 {
		t.Fatalf("в очереди %d байт при потолке %d", got, 1<<20)
	}
	if p.Dropped() == 0 {
		t.Fatal("отброшенные события не посчитаны — исчерпание бюджета не видно в самотелеметрии")
	}
}

// TestQueueByteBudgetReleasedAfterProcessing — бюджет возвращается по мере
// обработки, иначе очередь однажды заполнилась бы навсегда.
func TestQueueByteBudgetReleasedAfterProcessing(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.SetMaxQueueBytes(1 << 20)

	big := strings.Repeat("x", 256<<10)
	p.Enqueue(1, &ParsedEvent{EventID: "e", ContextsJSON: big})
	if p.QueuedBytes() == 0 {
		t.Fatal("постановка не зарезервировала бюджет")
	}

	// Разбираем очередь так же, как это делает воркер.
	task := <-p.queue
	p.processGuarded(task)

	if got := p.QueuedBytes(); got != 0 {
		t.Fatalf("после обработки в очереди осталось %d байт, want 0", got)
	}
}

// TestQueueByteBudgetSurvivesPanic — паника в обработке тоже возвращает
// бюджет: иначе очередь, пережившая несколько битых событий, навсегда считала
// бы себя заполненной, и приём вставал бы без единой причины в логе.
func TestQueueByteBudgetSurvivesPanic(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.SetMaxQueueBytes(1 << 20)

	// nil issues + непустое событие роняют process() внутри recover'а.
	p.Enqueue(1, &ParsedEvent{EventID: "boom", ContextsJSON: strings.Repeat("y", 1024)})
	task := <-p.queue
	p.processGuarded(task)

	if got := p.QueuedBytes(); got != 0 {
		t.Fatalf("после паники в очереди осталось %d байт, want 0", got)
	}
}

// TestTaskBytesCountsEmptyEvents — постоянная цена задачи не даёт обойти учёт
// потоком пустых событий (та же логика, что у rowOverheadBytes в батчере).
func TestTaskBytesCountsEmptyEvents(t *testing.T) {
	if got := taskBytes(task{ev: &ParsedEvent{}}); got <= 0 {
		t.Fatalf("пустое событие весит %d — учёт обходится потоком пустышек", got)
	}
}
