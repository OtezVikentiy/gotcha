package ingest

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
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
		p.Enqueue(1, 1, &ParsedEvent{EventID: "e", ContextsJSON: big})
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
	p.Enqueue(1, 1, &ParsedEvent{EventID: "e", ContextsJSON: big})
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
	p.Enqueue(1, 1, &ParsedEvent{EventID: "boom", ContextsJSON: strings.Repeat("y", 1024)})
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

// bigSpanData строит span.Data на грани капов transaction.go/otlp.go:
// maxDataKeys=64 ключей по maxDataValue=2000 рун строкового значения — то, ради
// чего байтовый бюджет очереди и заведён (см. taskBytes/dataMapBytes).
func bigSpanData() map[string]any {
	data := make(map[string]any, 64)
	val := strings.Repeat("d", 2000)
	for i := 0; i < 64; i++ {
		data[strings.Repeat("k", 10)+string(rune('a'+i))] = val
	}
	return data
}

// TestTaskBytesCountsSpanDataAndTags — находка P1-1: taskBytes раньше не
// считал ни sp.Data (map[string]any, «сырой JSON» из SDK — то самое поле, ради
// которого байтовый бюджет и заведён), ни tx.Tags. Транзакция с большими
// span.Data должна весить на порядки больше, чем те же поля без Data/Tags,
// иначе бюджет очереди слеп к реальному весу.
func TestTaskBytesCountsSpanDataAndTags(t *testing.T) {
	bare := trace.Transaction{
		Name: "tx", TraceID: "t", Environment: "prod",
		Spans: []trace.Span{{SpanID: "s", Op: "op", Description: "d", Status: "ok"}},
	}
	withData := bare
	withData.Tags = map[string]string{"team": strings.Repeat("x", 500)}
	withData.Spans = []trace.Span{{SpanID: "s", Op: "op", Description: "d", Status: "ok", Data: bigSpanData()}}

	bareBytes := taskBytes(task{tx: &bare})
	withDataBytes := taskBytes(task{tx: &withData})

	// 64 ключа × ~2010 байт (ключ+значение) — за сотню КиБ; голая транзакция —
	// пара сотен байт (taskOverheadBytes + короткие строки).
	if withDataBytes < bareBytes+100<<10 {
		t.Fatalf("taskBytes с span.Data(64×2000 рун)+tx.Tags = %d, без — %d: "+
			"разница подозрительно мала, Data/Tags не учитываются", withDataBytes, bareBytes)
	}
}

// TestQueueByteBudgetDropsOversizedSpanDataFlood — как
// TestQueueByteBudgetDropsOversizedFlood, но по EnqueueTransaction с большими
// span.Data: до фикса taskBytes не считал sp.Data вовсе, и такие транзакции
// проходили бюджет очереди почти бесплатно (≈256 байт учтённого веса при
// реальном весе за сотню КиБ на транзакцию).
func TestQueueByteBudgetDropsOversizedSpanDataFlood(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.SetMaxQueueBytes(1 << 20) // 1 МиБ

	tx := trace.Transaction{
		Name: "tx", TraceID: "t", Environment: "prod",
		Spans: []trace.Span{{SpanID: "s", Op: "op", Description: "d", Status: "ok", Data: bigSpanData()}},
	}
	accepted := 0
	for i := 0; i < 100; i++ {
		before := p.Queued()
		p.EnqueueTransaction(1, 1, tx)
		if p.Queued() > before {
			accepted++
		}
	}

	if accepted == 0 {
		t.Fatal("ни одна транзакция не принята — бюджет не пропускает вообще ничего")
	}
	// Каждая транзакция весит за 100 КиБ (64 ключа × ~2000 рун) — бюджет в
	// 1 МиБ пропускает единицы, а не все 100.
	if accepted > 20 {
		t.Fatalf("принято %d транзакций с большими span.Data при бюджете 1 МиБ — "+
			"похоже, sp.Data всё ещё не учитывается в весе задачи", accepted)
	}
	if got := p.QueuedBytes(); got > 1<<20 {
		t.Fatalf("в очереди %d байт при потолке %d", got, 1<<20)
	}
	if p.Dropped() == 0 {
		t.Fatal("отброшенные транзакции не посчитаны — исчерпание бюджета не видно в самотелеметрии")
	}
}
