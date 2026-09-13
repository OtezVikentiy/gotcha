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

type fakePerfSink struct {
	mu         sync.Mutex
	calls      int
	panics     bool
	err        error
	created    []bool // created для i-го вызова (по исчерпании — false)
	regression []bool // regression для i-го вызова (по исчерпании — false)
	recorded   []trace.Finding
	deadlines  []time.Time // дедлайн ctx на i-м вызове: общий бюджет — один на все находки
	delay      time.Duration
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

// покрывает гонку main.go: drain() закрывает очередь, пока in-flight обработчики
// зовут Enqueue.
func TestEnqueueAfterCloseDoesNotPanic(t *testing.T) {
	p := NewPipeline(nil, nil)
	p.Start()
	p.Close(context.Background())

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enqueue after Close panicked: %v", r)
		}
	}()
	if p.Enqueue(1, 1, &ParsedEvent{EventID: "x"}) {
		t.Error("Enqueue после Close вернул true, want false — задача не встала в очередь")
	}
	if p.EnqueueTransaction(1, 1, nPlusOneTx()) {
		t.Error("EnqueueTransaction после Close вернул true, want false — задача не встала в очередь")
	}
}

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

// func-обёртка вместо полноценного uptime.Service — реальный сервис с окнами
// обслуживания и своей БД тестам этого пакета не нужен.
type fakeMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (f fakeMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return f(ctx, projectID, at)
}

func TestPerfIssueMaintenanceSuppressesNotify(t *testing.T) {
	spans := &fakeSpanSink{}
	perf := &fakePerfSink{created: []bool{true}}
	notifier := &fakePerfNotifier{}

	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.PerfAlerts = notifier
	p.Maint = fakeMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil })
	p.Start()
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.Close(context.Background())

	if perf.calls != 1 {
		t.Fatalf("Record calls = %d, want 1 (сбор данных продолжается в окне обслуживания)", perf.calls)
	}
	if got := notifier.count(); got != 0 {
		t.Errorf("alerts = %d, want 0 (suppressed by maintenance)", got)
	}
}

func TestPerfIssueMaintenanceFalseStillNotifies(t *testing.T) {
	spans := &fakeSpanSink{}
	perf := &fakePerfSink{created: []bool{true}}
	notifier := &fakePerfNotifier{}

	p := NewPipeline(nil, nil)
	p.Spans = spans
	p.Perf = perf
	p.PerfAlerts = notifier
	p.Maint = fakeMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil })
	p.Start()
	p.EnqueueTransaction(1, 1, nPlusOneTx())
	p.Close(context.Background())

	if got := notifier.count(); got != 1 {
		t.Errorf("alerts = %d, want 1 (not suppressed outside maintenance)", got)
	}
}

func twoFindingTx() trace.Transaction {
	tx := nPlusOneTx()
	start := tx.Start
	tx.Spans = append(tx.Spans, trace.Span{
		SpanID: "slow", ParentSpanID: "root", Op: "db.sql.query",
		Description: "SELECT * FROM reports", Start: start, End: start.Add(900 * time.Millisecond),
	})
	return tx
}

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

// изображает деградировавший PostgreSQL: апсерт issue отваливается по таймауту.
type failingUpserter struct{ calls atomic.Int64 }

func (f *failingUpserter) Upsert(ctx context.Context, projectID int64, fingerprint, title, culprit, level, environment string, seenAt time.Time) (issue.UpsertResult, error) {
	f.calls.Add(1)
	return issue.UpsertResult{}, errors.New("timeout: context deadline exceeded")
}

func (f *failingUpserter) Get(ctx context.Context, issueID int64) (issue.Issue, error) {
	return issue.Issue{}, errors.New("timeout: context deadline exceeded")
}

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

type fakeDropCounter struct {
	mu            sync.Mutex
	events        map[int64]int64
	transactions  map[int64]int64
	metrics       map[int64]int64
	profiles      map[int64]int64
	logs          map[int64]int64
	metricsCalls  int
	profilesCalls int
	logsCalls     int
	// Месяцы фактических вызовов IncDropped* — проверить, что дроп относят к
	// своему месяцу, а не к месяцу флаша.
	eventMonths  []time.Time
	txMonths     []time.Time
	metricMonths []time.Time
}

func newFakeDropCounter() *fakeDropCounter {
	return &fakeDropCounter{
		events:       map[int64]int64{},
		transactions: map[int64]int64{},
		metrics:      map[int64]int64{},
		profiles:     map[int64]int64{},
		logs:         map[int64]int64{},
	}
}

// проверяет ctx.Err() первым, как реальный pgx-запрос с уже истёкшим ctx —
// иначе не отличить флаш со свежим контекстом от флаша с унаследованным истёкшим.
func (f *fakeDropCounter) IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[orgID] += n
	f.eventMonths = append(f.eventMonths, month)
	return nil
}

func (f *fakeDropCounter) IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transactions[orgID] += n
	f.txMonths = append(f.txMonths, month)
	return nil
}

func (f *fakeDropCounter) IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricsCalls++
	f.metrics[orgID] += n
	f.metricMonths = append(f.metricMonths, month)
	return nil
}

func (f *fakeDropCounter) IncDroppedProfiles(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profilesCalls++
	f.profiles[orgID] += n
	return nil
}

func (f *fakeDropCounter) IncDroppedLogs(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsCalls++
	f.logs[orgID] += n
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

func (f *fakeDropCounter) metricsFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metrics[orgID]
}

func (f *fakeDropCounter) profilesFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profiles[orgID]
}

func (f *fakeDropCounter) logsFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[orgID]
}

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
	// агрегат обязан обнуляться при флаше — иначе окно задваивалось бы на каждый следующий тик.
	p.flushDropped(context.Background())
	if got := dc.eventsFor(42); got != 5 {
		t.Errorf("повторный флаш изменил счётчик: got %d, want 5 (агрегат должен обнуляться при флаше)", got)
	}
	if dc.metricsCalls != 0 || dc.profilesCalls != 0 {
		t.Errorf("Pipeline вызвал IncDroppedMetrics/Profiles (%d/%d): он не видит эти классы, "+
			"их дроп-путь идёт мимо очереди", dc.metricsCalls, dc.profilesCalls)
	}
}

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

// Дроп из старого месяца, флашнутый уже в новом, обязан отчитаться в СВОЙ
// месяц — не в тот, что идёт на часах в момент флаша.
func TestPipelineFlushDroppedAttributesEachEntryToItsOwnMonth(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc

	oldMonth := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newMonth := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	// Как будто один дроп случился 31 января поздно вечером, другой — уже
	// 1 февраля, но оба долежали в агрегате до одного и того же флаша.
	p.dropAgg = map[dropAggKey]int64{
		{orgID: 1, kind: dropEvent, month: oldMonth}: 3,
		{orgID: 1, kind: dropEvent, month: newMonth}: 2,
	}

	p.flushDropped(context.Background())

	if got := dc.eventsFor(1); got != 5 {
		t.Fatalf("dropped events для org 1 = %d, want 5 (3+2)", got)
	}
	if len(dc.eventMonths) != 2 {
		t.Fatalf("IncDroppedEvents вызван %d раз, want 2 (по одному на месяц)", len(dc.eventMonths))
	}
	seen := map[time.Time]bool{dc.eventMonths[0]: true, dc.eventMonths[1]: true}
	if !seen[oldMonth] || !seen[newMonth] {
		t.Errorf("месяцы вызовов = %v, want ровно %v и %v — старый дроп не должен списаться в новый месяц",
			dc.eventMonths, oldMonth, newMonth)
	}
}

// Идёт обычным путём потери (CountDroppedEvents), не засевает dropAgg напрямую —
// иначе поломка dropMonthKey в самом countDroppedOrg осталась бы незамеченной.
func TestPipelineCountDroppedOrgStampsMonthAtDropTime(t *testing.T) {
	dc := newFakeDropCounter()
	p := NewPipeline(nil, nil)
	p.DropCounter = dc

	want := dropMonthKey(time.Now())
	p.CountDroppedEvents(1, 3)
	p.flushDropped(context.Background())

	if got := dc.eventsFor(1); got != 3 {
		t.Fatalf("dropped events = %d, want 3", got)
	}
	if len(dc.eventMonths) != 1 || !dc.eventMonths[0].Equal(want) {
		t.Fatalf("month = %v, want %v", dc.eventMonths, want)
	}
}

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
