package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// fakeSpanSink считает принятые транзакции — им проверяется главное свойство
// детекции: что бы в ней ни случилось, транзакция всё равно уезжает в CH.
type fakeSpanSink struct {
	mu    sync.Mutex
	added []trace.Transaction
}

func (f *fakeSpanSink) Add(_ int64, t trace.Transaction) {
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
		Issue:      trace.PerfIssue{ID: int64(i + 1), ProjectID: projectID, Kind: fi.Kind, Title: fi.Title},
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
	p.EnqueueTransaction(1, nPlusOneTx())
	p.EnqueueTransaction(1, nPlusOneTx())
	p.Close()

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
	p.EnqueueTransaction(1, nPlusOneTx())
	p.EnqueueTransaction(1, nPlusOneTx())
	p.EnqueueTransaction(1, nPlusOneTx())
	p.Close()

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
			p.EnqueueTransaction(1, nPlusOneTx())
			p.EnqueueTransaction(1, nPlusOneTx())
			p.Close()

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
	p.EnqueueTransaction(1, nPlusOneTx())
	p.Close()

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
	p.EnqueueTransaction(7, twoFindingTx())
	p.Close()

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
	p.EnqueueTransaction(7, twoFindingTx())
	p.Close()

	perf.mu.Lock()
	defer perf.mu.Unlock()
	if perf.calls != 1 {
		t.Fatalf("вызовов Record = %d, want 1: вторая находка должна отвалиться по общему бюджету", perf.calls)
	}
	if spans.count() != 1 {
		t.Errorf("транзакций в CH = %d, want 1: детекция не влияет на запись трейса", spans.count())
	}
}
