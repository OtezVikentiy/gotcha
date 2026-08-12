package ingest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// fakeSpanSink считает принятые транзакции — им проверяется главное свойство
// детекции: что бы в ней ни случилось, транзакция всё равно уезжает в CH.
type fakeSpanSink struct {
	mu    sync.Mutex
	added []trace.Transaction
}

func (f *fakeSpanSink) Add(_, _ int64, t trace.Transaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, t)
}

func (f *fakeSpanSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.added)
}

// fakePerfSink — PerfSink, который либо паникует, либо возвращает ошибку,
// либо считает вызовы Record и отдаёт created/regression по спискам.
type fakePerfSink struct {
	mu         sync.Mutex
	calls      int
	panics     bool
	err        error
	created    []bool // created для i-го вызова (по исчерпании — false)
	regression []bool // regression для i-го вызова (по исчерпании — false)
	recorded   []trace.Finding
	deadlines  []time.Time   // дедлайн ctx на i-м вызове: общий бюджет — один на все находки
	delay      time.Duration // сколько «работает» один Record
}

func (f *fakePerfSink) Record(ctx context.Context, projectID int64, fi trace.Finding, _ string) (trace.RecordResult, error) {
	dl, _ := ctx.Deadline()
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.recorded = append(f.recorded, fi)
	f.deadlines = append(f.deadlines, dl)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return trace.RecordResult{}, ctx.Err()
		}
	}

	if f.panics {
		panic("boom in detection")
	}
	if f.err != nil {
		return trace.RecordResult{}, f.err
	}
	return trace.RecordResult{
		Issue:      trace.PerfIssue{ID: int64(i + 1), ProjectID: projectID, Kind: fi.Kind, Description: fi.Description},
		Created:    i < len(f.created) && f.created[i],
		Regression: i < len(f.regression) && f.regression[i],
	}, nil
}

// fakePerfNotifier считает алерты о первом обнаружении и о регрессии.
type fakePerfNotifier struct {
	mu          sync.Mutex
	notified    int
	regressions int
}

func (f *fakePerfNotifier) NotifyNew(_ context.Context, _ int64, _ trace.PerfIssue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified++
	return nil
}

func (f *fakePerfNotifier) NotifyRegression(_ context.Context, _ int64, _ trace.PerfIssue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regressions++
	return nil
}

func (f *fakePerfNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notified
}

func (f *fakePerfNotifier) regressionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regressions
}

// nPlusOneTx — транзакция с NPlusOneMin (5) одинаковыми db-спанами под одним
// родителем: детектор обязан найти в ней ровно одну проблему.
func nPlusOneTx() trace.Transaction {
	start := time.Now().UTC()
	tx := trace.Transaction{
		TraceID: "t1", SpanID: "root", Name: "GET /api/users", Op: "http.server",
		Start: start, End: start.Add(500 * time.Millisecond),
	}
	for i := 0; i < 6; i++ {
		s := start.Add(time.Duration(i*10) * time.Millisecond)
		tx.Spans = append(tx.Spans, trace.Span{
			SpanID: string(rune('a' + i)), ParentSpanID: "root", Op: "db.sql.query",
			Description: "SELECT * FROM users WHERE id = 1", Start: s, End: s.Add(5 * time.Millisecond),
		})
	}
	return tx
}

// TestEnqueueAfterCloseDoesNotPanic покрывает гонку main.go: drain() закрывает
// очередь (Close), пока ещё не завершившиеся обработчики могут звать Enqueue.
// До фикса это паниковало (send on closed channel).
func TestEnqueueAfterCloseDoesNotPanic(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.Start()
	p.Close(context.Background())

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enqueue after Close panicked: %v", r)
		}
	}()
	p.Enqueue(1, 1, &ParsedEvent{EventID: "x"})
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
	p.Close(context.Background())
	p.Close(context.Background())
}

// TestTransactionDetectionAlertsOnlyOnFirstDetection: находка, увиденная
// впервые (created=true), шлёт алерт; та же находка второй раз — нет.
func TestTransactionDetectionAlertsOnlyOnFirstDetection(t *testing.T) {
	spans := &fakeSpanSink{}
	perf := &fakePerfSink{created: []bool{true}} // created только на первом Record
	notifier := &fakePerfNotifier{}

	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.PerfAlerts = notifier
	p.Start()
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.Close(context.Background())

	if spans.count() != 2 {
		t.Fatalf("spans added = %d, want 2", spans.count())
	}
	if perf.calls != 2 {
		t.Fatalf("Record calls = %d, want 2 (по одной находке n+1 на транзакцию)", perf.calls)
	}
	if perf.recorded[0].Kind != trace.KindNPlusOne {
		t.Errorf("recorded kind = %q, want %q", perf.recorded[0].Kind, trace.KindNPlusOne)
	}
	if got := notifier.count(); got != 1 {
		t.Errorf("alerts = %d, want 1 (только первое обнаружение)", got)
	}
}

// TestTransactionDetectionAlertsOnRegression: проблему пометили resolved, она
// вернулась — дежурный должен об этом узнать, а не обнаружить тихо переоткрытую
// проблему в списке (так же устроены алерты об ошибках, alert.KindRegression).
func TestTransactionDetectionAlertsOnRegression(t *testing.T) {
	spans := &fakeSpanSink{}
	// Первое обнаружение — новая проблема; второе — регрессия (Record вернул
	// created=false, regression=true); третье — обычный повтор, молчим.
	perf := &fakePerfSink{
		created:    []bool{true, false, false},
		regression: []bool{false, true, false},
	}
	notifier := &fakePerfNotifier{}

	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.PerfAlerts = notifier
	p.Start()
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.Close(context.Background())

	if got := notifier.count(); got != 1 {
		t.Errorf("алертов о новой проблеме = %d, want 1", got)
	}
	if got := notifier.regressionCount(); got != 1 {
		t.Errorf("алертов о регрессии = %d, want 1", got)
	}
}

// TestTransactionDetectionFailureDoesNotBreakIngest: паника и ошибка внутри
// детекции не должны ни ронять воркер, ни мешать записи транзакции в CH.
func TestTransactionDetectionFailureDoesNotBreakIngest(t *testing.T) {
	for _, tc := range []struct {
		name string
		perf *fakePerfSink
	}{
		{"panic", &fakePerfSink{panics: true}},
		{"error", &fakePerfSink{err: errors.New("pg is down")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spans := &fakeSpanSink{}
			p := NewPipeline(nil, nil)
			p.Spans = spans
			p.Perf = tc.perf
			p.PerfAlerts = &fakePerfNotifier{}
			p.Start()
			p.EnqueueTransaction(1, 1, nPlusOneTx())
			p.EnqueueTransaction(1, 1, nPlusOneTx())
			p.Close(context.Background())

			if spans.count() != 2 {
				t.Fatalf("spans added = %d, want 2: транзакция должна писаться в CH несмотря на сбой детекции", spans.count())
			}
		})
	}
}

// TestTransactionWithoutPerfSinkStillWrites — детекция необязательна:
// Perf == nil означает «детекторы выключены».
func TestTransactionWithoutPerfSinkStillWrites(t *testing.T) {
	spans := &fakeSpanSink{}
	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Start()
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.Close(context.Background())

	if spans.count() != 1 {
		t.Fatalf("spans added = %d, want 1", spans.count())
	}
}

// twoFindingTx — транзакция, дающая ДВЕ находки: N+1 и медленный запрос.
func twoFindingTx() trace.Transaction {
	tx := nPlusOneTx()
	start := tx.Start
	tx.Spans = append(tx.Spans, trace.Span{
		SpanID: "slow", ParentSpanID: "root", Op: "db.sql.query",
		Description: "SELECT * FROM reports", Start: start, End: start.Add(900 * time.Millisecond),
	})
	return tx
}

// Бюджет детекции ОДИН на всю транзакцию, а не на каждую находку: иначе
// транзакция с максимумом находок держала бы воркера ~100с, пока из той же
// очереди дропаются события об ошибках.
func TestPerfDetectionSharesOneBudget(t *testing.T) {
	spans := &fakeSpanSink{}
	perf := &fakePerfSink{}
	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.Start()
	p.EnqueueTransaction(7, 7, twoFindingTx())
	p.Close(context.Background())

	perf.mu.Lock()
	defer perf.mu.Unlock()
	if len(perf.deadlines) < 2 {
		t.Fatalf("вызовов Record = %d, want >= 2 (находки: %+v)", len(perf.deadlines), perf.recorded)
	}
	for i, dl := range perf.deadlines[1:] {
		if !dl.Equal(perf.deadlines[0]) {
			t.Fatalf("дедлайн находки %d = %v, у первой %v: бюджет должен быть ОДИН на всю детекцию",
				i+1, dl, perf.deadlines[0])
		}
	}
}

// Исчерпанный бюджет детекции: хвост находок пропускается (с warn-логом), а не
// удерживает воркера. Событиям об ошибках, идущим через ту же очередь, важнее
// живой воркер, чем полнота детекции.
func TestPerfDetectionStopsWhenBudgetExhausted(t *testing.T) {
	spans := &fakeSpanSink{}
	perf := &fakePerfSink{delay: 50 * time.Millisecond}
	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.testPerfBudget = 10 * time.Millisecond
	p.Start()
	p.EnqueueTransaction(7, 7, twoFindingTx())
	p.Close(context.Background())

	perf.mu.Lock()
	defer perf.mu.Unlock()
	if perf.calls != 1 {
		t.Fatalf("вызовов Record = %d, want 1: вторая находка должна отвалиться по общему бюджету", perf.calls)
	}
	if spans.count() != 1 {
		t.Errorf("транзакций в CH = %d, want 1: детекция не влияет на запись трейса", spans.count())
	}
}

// TestPipelineDropCounters фиксирует то, чего раньше не существовало: потери
// очереди ТОЛЬКО логировались и никуда не считались, поэтому оператор не мог
// узнать, что часть событий не доехала (org_usage.dropped_* их не видел).
func TestPipelineDropCounters(t *testing.T) {
	p := NewPipeline(nil, nil)
	// Воркеры НЕ запускаем: очередь никто не разбирает, значит переполнится.
	if got := p.QueueCap(); got <= 0 {
		t.Fatalf("QueueCap = %d, want > 0", got)
	}
	if got := p.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d на старте, want 0", got)
	}

	for i := 0; i < int(p.QueueCap())+50; i++ {
		p.Enqueue(1, 1, &ParsedEvent{EventID: "e"})
	}

	if got := p.Queued(); got != p.QueueCap() {
		t.Errorf("Queued = %d, want %d (очередь должна быть полна)", got, p.QueueCap())
	}
	dropped := p.Dropped()
	if dropped == 0 {
		t.Fatal("Dropped = 0: переполнение очереди обязано считаться, а не только логироваться")
	}
	if dropped != 50 {
		t.Errorf("Dropped = %d, want 50 (ровно столько не поместилось)", dropped)
	}
}

// failingUpserter изображает деградировавший PostgreSQL: апсерт issue
// отваливается по таймауту.
type failingUpserter struct{ calls atomic.Int64 }

func (f *failingUpserter) Upsert(ctx context.Context, projectID int64, fingerprint, title, culprit, level, environment string, seenAt time.Time) (issue.UpsertResult, error) {
	f.calls.Add(1)
	return issue.UpsertResult{}, errors.New("timeout: context deadline exceeded")
}

func (f *failingUpserter) Get(ctx context.Context, issueID int64) (issue.Issue, error) {
	return issue.Issue{}, errors.New("timeout: context deadline exceeded")
}

// TestStorageFailureCountsAsDrop: событие, выброшенное из-за отказа PostgreSQL,
// обязано попадать в счётчик потерь. Раньше оно только логировалось, а
// документация учила читать нулевой счётчик как «события не приходили» — и
// оператор при деградации базы уходил проверять SDK.
func TestStorageFailureCountsAsDrop(t *testing.T) {
	up := &failingUpserter{}
	p := NewPipeline(nil, nil)
	p.issues = up
	p.Start()
	p.Enqueue(1, 1, &ParsedEvent{EventID: "e1", Fingerprint: []string{"fp"}})
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if up.calls.Load() == 0 {
		t.Fatal("апсерт не вызывался — тест проверял бы не то")
	}
	if got := p.Dropped(); got != 1 {
		t.Errorf("Dropped = %d, want 1: событие потеряно из-за отказа хранилища", got)
	}
	if got := p.DroppedBy(DropStorageError); got != 1 {
		t.Errorf("DroppedBy(storage_error) = %d, want 1", got)
	}
	if got := p.DroppedBy(DropQueueFull); got != 0 {
		t.Errorf("DroppedBy(queue_full) = %d, want 0: причина потери определена неверно, "+
			"и оператор пойдёт увеличивать очередь вместо того, чтобы чинить базу", got)
	}
}

// TestDropReasonsAreDistinguishable: переполнение очереди и отказ хранилища
// лечатся по-разному, поэтому обязаны различаться в метрике.
func TestDropReasonsAreDistinguishable(t *testing.T) {
	p := NewPipeline(nil, nil)
	for i := 0; i < int(p.QueueCap())+7; i++ {
		p.Enqueue(1, 1, &ParsedEvent{EventID: "e"})
	}
	if got := p.DroppedBy(DropQueueFull); got != 7 {
		t.Errorf("DroppedBy(queue_full) = %d, want 7", got)
	}
	if got := p.DroppedBy(DropStorageError); got != 0 {
		t.Errorf("DroppedBy(storage_error) = %d, want 0", got)
	}
	if got := p.Dropped(); got != 7 {
		t.Errorf("Dropped = %d, want 7: сумма по причинам должна совпадать с общим счётчиком", got)
	}
}

// fakeDropCounter — DropCounter в памяти: считает вызовы по (orgID, kind), не
// трогая PostgreSQL. Реализует ingest.DropCounter — тот же интерфейс, что
// подставляется *org.Service и в Handler.DropCounter, и в Pipeline.DropCounter
// (см. w3-brief: пути не пересекаются, но делят интерфейс и реализацию).
type fakeDropCounter struct {
	mu            sync.Mutex
	events        map[int64]int64
	transactions  map[int64]int64
	metricsCalls  int
	profilesCalls int
}

func newFakeDropCounter() *fakeDropCounter {
	return &fakeDropCounter{events: map[int64]int64{}, transactions: map[int64]int64{}}
}

// IncDroppedEvents/IncDroppedTransactions проверяют ctx.Err() ПЕРВЫМ делом,
// как это сделал бы реальный pgx-запрос с уже отменённым/истёкшим ctx —
// без этого fakeDropCounter не отличил бы флаш со свежим контекстом от флаша
// с унаследованным истёкшим (см. TestPipelineDropFlushSurvivesDrainTimeout,
// которая иначе проходила бы и с багом, и без него).
func (f *fakeDropCounter) IncDroppedEvents(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[orgID] += n
	return nil
}

func (f *fakeDropCounter) IncDroppedTransactions(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transactions[orgID] += n
	return nil
}

// IncDroppedMetrics/IncDroppedProfiles — Pipeline не должен звать их вовсе
// (метрики/профили идут мимо очереди, см. handler.go); тесты ниже проверяют
// счётчики вызовов, чтобы это осталось так.
func (f *fakeDropCounter) IncDroppedMetrics(_ context.Context, _ int64, _ time.Time, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricsCalls++
	return nil
}

func (f *fakeDropCounter) IncDroppedProfiles(_ context.Context, _ int64, _ time.Time, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profilesCalls++
	return nil
}

func (f *fakeDropCounter) eventsFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[orgID]
}

func (f *fakeDropCounter) txFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transactions[orgID]
}

// TestPipelineFlushesDropsPerOrg — находка w3 (P1): пайплайн-дропы (переполнение
// очереди — уже ПОСЛЕ того, как grant списал квоту) раньше не попадали в
// org_usage.dropped_* вовсе, только в process-local Pipeline.dropped.
// Агрегация копится в памяти между флашами (не пишет в БД на каждый дроп —
// см. докблок Pipeline.DropCounter) и сливается по вызову flushDropped.
func TestPipelineFlushesDropsPerOrg(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	// Воркеры не запускаем: очередь переполнится детерминированно, как в
	// TestPipelineDropCounters.
	for i := 0; i < int(p.QueueCap())+5; i++ {
		p.Enqueue(1, 42, &ParsedEvent{EventID: "e"})
	}
	if got := dc.eventsFor(42); got != 0 {
		t.Fatalf("dropped events для org 42 ДО флаша = %d, want 0: агрегация обязана копиться в "+
			"памяти между флашами, а не писать в DropCounter синхронно на каждый дроп", got)
	}
	p.flushDropped(context.Background())
	if got := dc.eventsFor(42); got != 5 {
		t.Errorf("dropped events для org 42 после флаша = %d, want 5 (ровно столько не поместилось)", got)
	}
	// Повторный флаш без новых дропов ничего не добавляет — агрегат обязан
	// обнуляться при флаше (drainDropAgg), иначе окно задваивалось бы на
	// каждый следующий тик.
	p.flushDropped(context.Background())
	if got := dc.eventsFor(42); got != 5 {
		t.Errorf("повторный флаш изменил счётчик: got %d, want 5 (агрегат должен обнуляться при флаше)", got)
	}
	if dc.metricsCalls != 0 || dc.profilesCalls != 0 {
		t.Errorf("Pipeline вызвал IncDroppedMetrics/Profiles (%d/%d): он не видит эти классы, "+
			"их дроп-путь идёт мимо очереди", dc.metricsCalls, dc.profilesCalls)
	}
}

// TestPipelineFlushesTransactionDropsPerOrg — то же самое для транзакций
// (dropTransaction), отдельный класс и отдельный счётчик org_usage.
func TestPipelineFlushesTransactionDropsPerOrg(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	for i := 0; i < int(p.QueueCap())+3; i++ {
		p.EnqueueTransaction(1, 7, nPlusOneTx())
	}
	p.flushDropped(context.Background())
	if got := dc.txFor(7); got != 3 {
		t.Errorf("dropped transactions для org 7 = %d, want 3", got)
	}
	if got := dc.eventsFor(7); got != 0 {
		t.Errorf("dropped events для org 7 = %d, want 0: транзакции не должны попадать в счётчик событий", got)
	}
}

// TestPipelineSkipsOrgAttributionWithoutOrgID — orgID<=0 (вызывающий не провёл
// организацию) не должен уйти в DropCounter ни под каким ключом: атрибутировать
// некуда. Process-local счётчик (см. Pipeline.dropped) при этом не зависит от
// orgID и продолжает считать как раньше.
func TestPipelineSkipsOrgAttributionWithoutOrgID(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	for i := 0; i < int(p.QueueCap())+4; i++ {
		p.Enqueue(1, 0, &ParsedEvent{EventID: "e"})
	}
	p.flushDropped(context.Background())
	if got := dc.eventsFor(0); got != 0 {
		t.Errorf("dropped events для orgID=0 = %d, want 0: атрибуции без orgID быть не должно", got)
	}
	if got := p.Dropped(); got != 4 {
		t.Errorf("process-local Dropped = %d, want 4: он не зависит от orgID", got)
	}
}

// TestPipelineDropFlushOnClose — Close обязан слить накопленное ПЕРЕД
// остановкой процесса: dropAgg живёт только в памяти, и без финального флаша
// последнее окно потерь исчезало бы бесследно (в отличие от process-local
// Pipeline.dropped, который для отчёта оператору не нужен после смерти процесса).
func TestPipelineDropFlushOnClose(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	p.SetMaxQueueBytes(1) // бюджет меньше цены любой задачи — первое же Enqueue не пройдёт
	p.Start()
	p.Enqueue(1, 99, &ParsedEvent{EventID: "e", ContextsJSON: "x"})
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := dc.eventsFor(99); got != 1 {
		t.Errorf("dropped events для org 99 после Close = %d, want 1: "+
			"Close обязан сделать финальный флаш, не дожидаясь тика", got)
	}
}

// TestHandlerQuotaDropsAndPipelineDropsDoNotOverlap — верификация из w3-brief:
// Handler.countDrop (квотные отказы, ДО постановки в очередь) и
// Pipeline.countDroppedOrg (потери самой очереди/обработки, ПОСЛЕ того как
// grant уже списал квоту) — это две ТОЧКИ ЖИЗНЕННОГО ЦИКЛА, которые не
// пересекаются. Общий DropCounter получает вклад от обеих и просто складывает
// — задвоения быть не должно.
func TestHandlerQuotaDropsAndPipelineDropsDoNotOverlap(t *testing.T) {
	dc := newFakeDropCounter()

	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	for i := 0; i < int(p.QueueCap())+3; i++ {
		p.Enqueue(1, 5, &ParsedEvent{EventID: "e"}) // 3 потери пайплайна (очередь полна)
	}
	p.flushDropped(context.Background())

	h := &Handler{DropCounter: dc}
	h.countDrop(context.Background(), dropEvent, 5, 2) // 2 потери квоты (envelope/store)

	if got := dc.eventsFor(5); got != 5 {
		t.Fatalf("dropped events для org 5 = %d, want 5 (3 пайплайна + 2 квота): "+
			"пути не пересекаются — счётчик обязан складывать, а не задваивать", got)
	}
}

// TestPipelineDropFlushSurvivesDrainTimeout — код-ревью (w3): финальный флаш в
// Close ДО фикса наследовал ctx дренажа, а на пути таймаута дренажа этот ctx
// уже Done() — WithTimeout от него отменён немедленно, и IncDropped* отваливался
// бы, теряя ровно то окно, которое финальный флаш обязан был спасти. Тест
// намеренно доводит Close до ветки таймаута (воркер занят дольше дедлайна
// Close) и проверяет, что дроп, поставленный ДО Close, всё равно долетает.
func TestPipelineDropFlushSurvivesDrainTimeout(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc
	p.Spans = &fakeSpanSink{}
	// Держит воркера занятым дольше дедлайна Close ниже — это и обязано
	// протолкнуть Close в ветку "drain timed out".
	p.Perf = &fakePerfSink{delay: 200 * time.Millisecond}
	p.Start()

	// Дроп ДО начала drain: байтовый бюджет в 1 байт не пропускает событие с
	// непустым ContextsJSON — детерминированно, без зависимости от воркеров.
	p.SetMaxQueueBytes(1)
	p.Enqueue(1, 55, &ParsedEvent{EventID: "e", ContextsJSON: "x"})
	// Возвращаем нормальный бюджет и грузим воркера транзакцией с задержкой в
	// детекции — на время её обработки Close должен успеть упереться в дедлайн.
	p.SetMaxQueueBytes(1 << 20)
	p.EnqueueTransaction(1, 55, nPlusOneTx())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := p.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err = %v, want context.DeadlineExceeded: тест не попал в ветку таймаута "+
			"дренажа, которую и должен проверять", err)
	}
	if got := dc.eventsFor(55); got != 1 {
		t.Errorf("dropped events для org 55 после Close с истёкшим ctx = %d, want 1: "+
			"финальный флаш обязан использовать свой независимый контекст, а не унаследованный истёкший", got)
	}
}
