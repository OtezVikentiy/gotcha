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

// stepSatEventSink и stepSatSpanSink — двойники eventSink/SpanSink с
// насыщенностью, управляемой ЧИСЛОМ ОПРОСОВ Saturation(), а не временем: тест
// ожидания (waitForRoom, T6) не имеет права зависеть от реальных
// миллисекунд — только от факта и порядка (см. бриф T6, "не флейкать на
// загруженной машине"). freeAfter — после какого по счёту вызова Saturation()
// приёмник считается свободным (возвращает 0.5); freeAfter<=0 — не
// освобождается никогда, имитируя буфер, который флашер не разгружает вовсе
// за время теста.
//
// Соседи по пакету уже заводят двойники с ФИКСИРОВАННОЙ насыщенностью
// (satEventSink/satSpanSink в overload_internal_test.go, sat всегда одно и то
// же значение) — этим тестам нужен переход состояния, отдельного типа для
// которого там нет, и не должно быть: тот файл проверяет preflight хендлера
// и мутировать посторонний тест ради этого файла — не задача T6.
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

// TestBackpressureWaitsThenWritesEvent: батчер насыщен на постановку задачи и
// освобождается ДО истечения бюджета ожидания — событие обязано доехать (без
// потери, drop-oldest тут ни при чём) и счётчик ожиданий обязан вырасти
// ровно на одно. checks.Load() > freeAfter — прямое доказательство, что
// воркер реально опрашивал насыщенность несколько раз (ждал), а не проскочил
// с одной проверки: без этого мутация "убрать цикл ожидания, оставить только
// счётчик" осталась бы незамеченной.
func TestBackpressureWaitsThenWritesEvent(t *testing.T) {
	sink := &stepSatEventSink{freeAfter: 3}
	p := NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	p.batcher = sink
	p.testBackpressureBudget = time.Second
	p.testBackpressurePoll = 2 * time.Millisecond
	p.Start()

	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})

	// Дожидаемся записи ПОЛЛИНГОМ, а не через Close: Close сам взводит
	// p.stopping и оборвал бы ожидание на ближайшем тике (см. отдельный тест
	// на остановку ниже), что для ЭТОГО теста подменило бы проверяемый путь —
	// "дождался освобождения" на "прервали остановкой". Потолок (300мс)
	// на два порядка больше ожидаемых ~3 опросов по 2мс, устойчив к
	// загруженной машине, но много меньше testBackpressureBudget (1с) —
	// добор до потолка сигналил бы, что освобождение вообще не подхватилось.
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
	// Диапазон, не точная величина: сама длительность ожидания не
	// детерминирована (зависит от планировщика ОС), но она не может
	// превысить testBackpressureBudget (1с) — waitForRoom не ждёт дольше
	// deadline.C. Верхняя граница 1.5с даёт запас на загруженную машину и
	// одновременно ловит подмену секунд наносекундами: BackpressureWaitSeconds,
	// вернувший наносекунды под видом секунд, здесь оказался бы порядка
	// 10^6-10^7, а не долей секунды.
	if got := p.BackpressureWaitSeconds(); got <= 0 || got > 1.5 {
		t.Errorf("BackpressureWaitSeconds() = %v, want в (0, 1.5]: величина "+
			"обязана быть в секундах, не в наносекундах (мутация: метрика "+
			"возвращает наносекунды под именем seconds?)", got)
	}
}

// TestBackpressureWaitsThenWritesTransaction — то же, что
// TestBackpressureWaitsThenWritesEvent, но по пути транзакций (p.Spans), а не
// событий: воркер ждёт SpanSink так же, как батчер, независимым вызовом
// waitForRoom в processTransaction.
func TestBackpressureWaitsThenWritesTransaction(t *testing.T) {
	sink := &stepSatSpanSink{freeAfter: 3}
	p := NewPipeline(nil, nil)
	p.Spans = sink
	p.testBackpressureBudget = time.Second
	p.testBackpressurePoll = 2 * time.Millisecond
	p.Start()

	p.EnqueueTransaction(1, 1, nPlusOneTx())

	// См. докблок TestBackpressureWaitsThenWritesEvent — поллинг, а не Close,
	// иначе проверяем не тот путь освобождения.
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

// TestBackpressureBudgetExpiresEventStillWrites: батчер остаётся насыщенным
// дольше всего бюджета ожидания — drop-oldest остаётся последним рубежом
// (решение владельца, T6), задача обязана дойти до Add, а воркер обязан
// продолжить работу, а не зависнуть навсегда. Close НЕ вызывается, пока
// событие не дойдёт (или не истечёт локальный потолок теста) — иначе
// установка p.stopping сама оборвала бы ожидание, и тест перестал бы отличать
// "сработал бюджет" от "сработала остановка" (см. отдельный тест на
// остановку ниже). Потолок опроса (300мс) заведомо больше тестового бюджета
// (40мс) на порядок — устойчиво к загруженной машине, но ловит мутацию
// "бюджет бесконечен".
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

// TestBackpressureNoWaitWhenNotSaturatedEvent: приёмник НЕ насыщен —
// waitForRoom обязан вернуться немедленно, ни разу не войдя в цикл опроса.
// Проверяется не временем (не флейкает), а тем, что счётчик ожиданий вообще
// не двигается — сторож на удорожание горячего пути.
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

// TestBackpressureNoWaitWithoutSaturationSourceEvent: батчер без метода
// Saturation() (двойник, не реализующий saturationSource, — как и до T6) не
// должен ждать вовсе: saturationOf возвращает для него 0. Поведение путей без
// этой способности не меняется правкой T6.
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

// TestBackpressureNoWaitWithoutSaturationSourceTransaction — то же самое для
// SpanSink: fakeSpanSink (pipeline_unit_test.go) не реализует Saturation().
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

// TestBackpressureCloseInterruptsWaitEvent — САМЫЙ ВАЖНЫЙ тест правки:
// остановка обязана обрывать ожидание немедленно, а не тянуть его до
// бюджета. Бюджет теста заведомо большой (2с) — если сигнал остановки
// сломан (или удалён), Close встанет минимум на этот бюджет на КАЖДОЙ задаче
// из очереди, и явный потолок ниже (500мс, на порядок меньше бюджета —
// устойчиво к загруженной машине) поймает это как таймаут, а не как флейк по
// миллисекундам. Без этого теста регресс "дренаж встал на 90 секунд" приехал
// бы на прод незамеченным (см. бриф T6) — Close(ctx) в проде вызывается с
// ctx, ограниченным stop_grace_period, но сам per-task бюджет ожидания
// живёт ВНЕ этого ctx (см. докблок waitForRoom), и без p.stopping ничего его
// не оборвало бы раньше него.
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
	// СВОЙ context.Background(), а не таймаут: если бы Close сам себя
	// ограничивал ctx, тест ловил бы просто "Close вернулся по ctx.Done()"
	// — не то же самое, что "ожидание оборвалось сигналом остановки". Драп
	// сам не теряет задачу на этом пути (см. ассерт ниже), только на
	// внешнем ctx-таймауте (drainErr), которого здесь нет.
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

// TestBackpressureCloseInterruptsWaitTransaction — то же самое для p.Spans.
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

// TestBackpressureCloseInterruptsWaitOnLiteralPipeline — ревью T6, находка 1:
// p.stopping обязан прерывать ожидание одинаково у Pipeline, собранного
// NewPipeline, и у Pipeline{}, собранного тестовым литералом в обход
// конструктора (как в scrub_integration_test.go). До этой правки остановка
// была каналом stopWait, полем, инициализируемым ТОЛЬКО в NewPipeline: у
// литерала он оставался nil, select на нём не срабатывал никогда, и Close
// такого пайплайна вставал на весь backpressureBudget() на каждой
// насыщенной задаче вместо немедленного возврата — то есть литерал вёл себя
// иначе, чем пайплайн, собранный конструктором, ровно тот инвариант, что
// документирован у queueLimit/SetMaxQueueBytes ("Pipeline остаётся
// собираемым литералом"). Потолок (500мс, как у остальных тестов на
// остановку в этом файле) на порядок меньше тестового бюджета (2с) —
// устойчив к загруженной машине, но ловит регресс за секунды, а не за
// таймаут всего прогона.
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
	// Даём воркеру время забрать задачу из очереди и войти в waitForRoom
	// ДО Close — иначе Close мог бы застать задачу ещё не взятой в работу,
	// и тест перестал бы отличать "прервали ожидание" от "не успели начать".
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
