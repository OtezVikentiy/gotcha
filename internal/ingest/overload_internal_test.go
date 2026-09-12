package ingest

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pp "github.com/google/pprof/profile"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// реализуют saturationSource поверх обычного sink-контракта — насыщенность
// подставляется тестом, не гоняется настоящим буфером до потолка.
type satEventSink struct {
	sat float64

	mu    sync.Mutex
	added []event.Event
}

func (s *satEventSink) Add(e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = append(s.added, e)
}
func (s *satEventSink) Saturation() float64 { return s.sat }
func (s *satEventSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.added)
}

type satSpanSink struct {
	sat float64

	mu    sync.Mutex
	added []trace.Transaction
}

func (s *satSpanSink) Add(_, _ int64, t trace.Transaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = append(s.added, t)
}
func (s *satSpanSink) Saturation() float64 { return s.sat }
func (s *satSpanSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.added)
}

type satMetricSink struct {
	sat float64

	mu     sync.Mutex
	points []metric.MetricPoint
}

func (s *satMetricSink) Add(_ int64, p metric.MetricPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.points = append(s.points, p)
}
func (s *satMetricSink) Saturation() float64 { return s.sat }
func (s *satMetricSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.points)
}

type satLogSink struct {
	sat float64

	mu      sync.Mutex
	records []log.LogRecord
}

func (s *satLogSink) Add(_ int64, r log.LogRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}
func (s *satLogSink) Saturation() float64 { return s.sat }
func (s *satLogSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

type satProfileSink struct {
	sat float64

	mu   sync.Mutex
	pros []profile.Profile
}

func (s *satProfileSink) Add(_ int64, p profile.Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pros = append(s.pros, p)
}
func (s *satProfileSink) Saturation() float64 { return s.sat }
func (s *satProfileSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pros)
}

// overloaded preflight обязан отбивать запрос до h.grant — иначе организация
// заплатила бы квотой дважды: за элемент из буфера и при ретрае на 503.
type countingQuota struct {
	mu    sync.Mutex
	calls int
}

func (q *countingQuota) CheckAndCount(context.Context, int64, int64) (int64, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls++
	return 1 << 30, time.Time{}, nil
}

// эти тесты проверяют только CheckAndCount — возврат в сценариях файла не наступает.
func (q *countingQuota) Refund(context.Context, int64, int64, time.Time) error { return nil }

func (q *countingQuota) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

// рабочий фейк: process() зовёт p.issues.Upsert безусловно, nil-интерфейс запаниковал бы.
func newSatPipeline(eventSat, txSat float64) (p *Pipeline, ev *satEventSink, sp *satSpanSink) {
	p = NewPipeline(nil, nil)
	p.issues = &fakeIssueSvc{res: issue.UpsertResult{IssueID: 1}}
	ev = &satEventSink{sat: eventSat}
	sp = &satSpanSink{sat: txSat}
	p.batcher = ev
	p.Spans = sp
	return p, ev, sp
}

func overloadKeyCache() *KeyCache {
	return NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}})
}

func TestSaturationOf(t *testing.T) {
	if got := saturationOf(nil); got != 0 {
		t.Errorf("saturationOf(nil) = %v, want 0", got)
	}
	if got := saturationOf(&collectMetricSink{}); got != 0 {
		t.Errorf("saturationOf(collectMetricSink без Saturation) = %v, want 0", got)
	}
	if got := saturationOf(&collectLogSink{}); got != 0 {
		t.Errorf("saturationOf(collectLogSink без Saturation) = %v, want 0", got)
	}
	if got := saturationOf(&fakeSpanSink{}); got != 0 {
		t.Errorf("saturationOf(fakeSpanSink без Saturation) = %v, want 0", got)
	}
	if got := saturationOf(&fakeBatcher{}); got != 0 {
		t.Errorf("saturationOf(fakeBatcher без Saturation) = %v, want 0", got)
	}
	if got := saturationOf(&satMetricSink{sat: 0.42}); got != 0.42 {
		t.Errorf("saturationOf(satMetricSink{0.42}) = %v, want 0.42", got)
	}
}

func TestPipelineEventTransactionSaturation(t *testing.T) {
	p, ev, sp := newSatPipeline(0, 0)
	if got := p.EventSaturation(); got != 0 {
		t.Errorf("EventSaturation() = %v на пустой очереди и пустом батчере, want 0", got)
	}
	if got := p.TransactionSaturation(); got != 0 {
		t.Errorf("TransactionSaturation() = %v, want 0", got)
	}

	ev.sat = 0.8
	sp.sat = 0.9
	if got := p.EventSaturation(); got != 0.8 {
		t.Errorf("EventSaturation() = %v, want 0.8 (максимум по батчеру)", got)
	}
	if got := p.TransactionSaturation(); got != 0.9 {
		t.Errorf("TransactionSaturation() = %v, want 0.9 (максимум по SpanWriter)", got)
	}

	p.Spans = nil
	if got := p.TransactionSaturation(); got != 0 {
		t.Errorf("TransactionSaturation() с выключенным трейсингом = %v, want 0", got)
	}
}

func TestNewPipelineNilBatcherStaysNilInterface(t *testing.T) {
	p := NewPipeline(nil, nil)
	if p.batcher != nil {
		t.Fatalf("p.batcher = %#v, want настоящий nil-интерфейс", p.batcher)
	}
	if got := p.EventSaturation(); got != 0 {
		t.Errorf("EventSaturation() с nil-батчером = %v, want 0 (не паника)", got)
	}
}

func TestOverloadedThreshold(t *testing.T) {
	h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)

	w := httptest.NewRecorder()
	if !h.overloaded(w, 1, 1, SignalEvent, overloadThreshold) {
		t.Fatal("overloaded(0.95) = false, want true (порог включительно)")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(w.Body.String(), "ingest overloaded") {
		t.Errorf("тело = %s, want detail=ingest overloaded", w.Body.String())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1", got)
	}

	w2 := httptest.NewRecorder()
	if h.overloaded(w2, 1, 1, SignalEvent, 0.9499) {
		t.Fatal("overloaded(0.9499) = true, want false (ниже порога)")
	}
	if w2.Code != http.StatusOK { // httptest.ResponseRecorder по умолчанию 200, если ничего не писали
		t.Errorf("status = %d при непринятом отказе, ответ не должен был писаться", w2.Code)
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1 (не выросло на непринятом отказе)", got)
	}
}

// без троттлинга overloaded сам стал бы нагрузкой, когда система не справляется —
// RejectedBy растёт без пропусков, троттлинг касается только лога.
func TestOverloadedLogThrottled(t *testing.T) {
	h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		if !h.overloaded(w, 1, 1, SignalEvent, 1.0) {
			t.Fatalf("итерация %d: overloaded = false, want true", i)
		}
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 3 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 3 (счётчик растёт на каждый отказ, троттлинг лога его не касается)", got)
	}
	if got := strings.Count(logs.String(), "buffer overloaded"); got != 1 {
		t.Errorf("строк лога = %d, want 1 (троттлинг: не чаще раза в overloadLogInterval)", got)
	}

	w := httptest.NewRecorder()
	if !h.overloaded(w, 1, 1, SignalTransaction, 1.0) {
		t.Fatal("overloaded(transaction) = false, want true")
	}
	if got := strings.Count(logs.String(), "buffer overloaded"); got != 2 {
		t.Errorf("строк лога после отказа по ДРУГОМУ signal'у = %d, want 2 (троттлинг per-signal, а не общий)", got)
	}

	// Интервал истёк — симулируем сдвигом сохранённого времени в прошлое, не
	// ждём реальные 5с: следующий отказ по event обязан снова залогироваться.
	h.overloadLogMu.Lock()
	h.lastOverloadLog[SignalEvent] = time.Now().Add(-overloadLogInterval - time.Second)
	h.overloadLogMu.Unlock()
	w = httptest.NewRecorder()
	if !h.overloaded(w, 1, 1, SignalEvent, 1.0) {
		t.Fatal("overloaded(event) после истечения интервала = false, want true")
	}
	if got := strings.Count(logs.String(), "buffer overloaded"); got != 3 {
		t.Errorf("строк лога после истечения интервала = %d, want 3", got)
	}
}

func TestOverloadStore(t *testing.T) {
	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/api/1/store/?sentry_key=pub", strings.NewReader("{}"))
		req.SetPathValue("project", "1")
		return req
	}

	t.Run("saturated", func(t *testing.T) {
		p, ev, _ := newSatPipeline(1.0, 0)
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), q, p, 1<<20)
		p.Start()
		w := httptest.NewRecorder()
		h.store(w, newReq())
		p.Close(context.Background())

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if ev.count() != 0 {
			t.Errorf("батчер увидел %d событий, want 0", ev.count())
		}
		if q.count() != 0 {
			t.Errorf("квота вызвана %d раз, want 0 — 503 обязан отбивать ДО h.grant", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
			t.Errorf("RejectedBy(overloaded, event) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		p, ev, _ := newSatPipeline(0.5, 0)
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		p.Start()
		w := httptest.NewRecorder()
		h.store(w, newReq())
		p.Close(context.Background())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if ev.count() != 1 {
			t.Errorf("батчер увидел %d событий, want 1", ev.count())
		}
	})
}

func postTraces(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	now := time.Now().UTC()
	span := &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /x",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(now.Add(-time.Second)),
		EndTimeUnixNano:   nanos(now),
	}
	raw, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{resSpans(nil, span)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpTraces(w, req)
	return w
}

func TestOverloadOTLPTraces(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		p, _, sp := newSatPipeline(0, 1.0)
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		h.TxQuota = q
		p.Start()
		w := postTraces(t, h)
		p.Close(context.Background())

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if sp.count() != 0 {
			t.Errorf("SpanWriter увидел %d транзакций, want 0", sp.count())
		}
		if q.count() != 0 {
			t.Errorf("TxQuota вызвана %d раз, want 0", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalTransaction); got != 1 {
			t.Errorf("RejectedBy(overloaded, transaction) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		p, _, sp := newSatPipeline(0, 0.5)
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		p.Start()
		w := postTraces(t, h)
		p.Close(context.Background())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if sp.count() != 1 {
			t.Errorf("SpanWriter увидел %d транзакций, want 1", sp.count())
		}
	})
}

func gaugeResourceMetrics() []*metricspb.ResourceMetrics {
	return []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cpu", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(time.Now().UnixNano()),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5},
				}},
			}}},
		}}},
	}}
}

func TestOverloadOTLPMetrics(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		sink := &satMetricSink{sat: 1.0}
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Metrics = sink
		h.MetricQuota = q
		w := postOTLPMetrics(t, h, gaugeResourceMetrics())

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if sink.count() != 0 {
			t.Errorf("MetricSink увидел %d точек, want 0", sink.count())
		}
		if q.count() != 0 {
			t.Errorf("MetricQuota вызвана %d раз, want 0", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalMetric); got != 1 {
			t.Errorf("RejectedBy(overloaded, metric) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		sink := &satMetricSink{sat: 0.5}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Metrics = sink
		w := postOTLPMetrics(t, h, gaugeResourceMetrics())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if sink.count() != 1 {
			t.Errorf("MetricSink увидел %d точек, want 1", sink.count())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		w := postOTLPMetrics(t, h, gaugeResourceMetrics())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalMetric); got != 0 {
			t.Errorf("RejectedBy(overloaded, metric) = %d, want 0 (выключенный сигнал — не отказ)", got)
		}
	})
}

func TestOverloadOTLPLogs(t *testing.T) {
	rl := []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
	}}

	t.Run("saturated", func(t *testing.T) {
		sink := &satLogSink{sat: 1.0}
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Logs = sink
		h.LogQuota = q
		w := postOTLPLogs(t, h, rl)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if sink.count() != 0 {
			t.Errorf("LogSink увидел %d записей, want 0", sink.count())
		}
		if q.count() != 0 {
			t.Errorf("LogQuota вызвана %d раз, want 0", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalLog); got != 1 {
			t.Errorf("RejectedBy(overloaded, log) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		sink := &satLogSink{sat: 0.5}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Logs = sink
		w := postOTLPLogs(t, h, rl)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if sink.count() != 1 {
			t.Errorf("LogSink увидел %d записей, want 1", sink.count())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		w := postOTLPLogs(t, h, rl)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalLog); got != 0 {
			t.Errorf("RejectedBy(overloaded, log) = %d, want 0 (выключенный сигнал — не отказ)", got)
		}
	})
}

func TestOverloadLogsNDJSON(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		sink := &satLogSink{sat: 1.0}
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Logs = sink
		h.LogQuota = q
		w := postNDJSON(t, h, `{"message":"hi"}`+"\n", false)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if sink.count() != 0 {
			t.Errorf("LogSink увидел %d записей, want 0", sink.count())
		}
		if q.count() != 0 {
			t.Errorf("LogQuota вызвана %d раз, want 0", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalLog); got != 1 {
			t.Errorf("RejectedBy(overloaded, log) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		sink := &satLogSink{sat: 0.5}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Logs = sink
		w := postNDJSON(t, h, `{"message":"hi"}`+"\n", false)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if sink.count() != 1 {
			t.Errorf("LogSink увидел %d записей, want 1", sink.count())
		}
	})
}

// google/pprof/profile.Profile.Write сам жмёт gzip'ом — pprofIngest ждёт тело
// именно в этом виде без Content-Encoding.
func validPprofGzip(t *testing.T) []byte {
	t.Helper()
	p := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Sample:     []*pp.Sample{{Value: []int64{1}}},
		TimeNanos:  time.Now().UnixNano(),
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatalf("pprof write: %v", err)
	}
	return buf.Bytes()
}

func TestOverloadPprof(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		sink := &satProfileSink{sat: 1.0}
		q := &countingQuota{}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Profiles = sink
		h.ProfileQuota = q
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(bytes.NewReader(validPprofGzip(t)), ""))

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if sink.count() != 0 {
			t.Errorf("ProfileSink увидел %d профилей, want 0", sink.count())
		}
		if q.count() != 0 {
			t.Errorf("ProfileQuota вызвана %d раз, want 0", q.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalProfile); got != 1 {
			t.Errorf("RejectedBy(overloaded, profile) = %d, want 1", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		sink := &satProfileSink{sat: 0.5}
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		h.Profiles = sink
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(bytes.NewReader(validPprofGzip(t)), ""))

		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202, body=%s", w.Code, w.Body.String())
		}
		if sink.count() != 1 {
			t.Errorf("ProfileSink увидел %d профилей, want 1", sink.count())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		h := NewHandler(overloadKeyCache(), nil, nil, 1<<20)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(bytes.NewReader(validPprofGzip(t)), ""))

		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202, body=%s", w.Code, w.Body.String())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalProfile); got != 0 {
			t.Errorf("RejectedBy(overloaded, profile) = %d, want 0 (выключенный сигнал — не отказ)", got)
		}
	})
}

const envelopeTxItem = `{"type":"transaction"}
{"transaction":"GET /x","contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","span_id":"bbbbbbbbbbbbbbbb"}}}
`

func envelopeRequest(body string) *http.Request {
	req := httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=pub", strings.NewReader(body))
	req.SetPathValue("project", "1")
	return req
}

func TestOverloadEnvelopeMixed(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem

	t.Run("события насыщены, транзакции свободны", func(t *testing.T) {
		p, ev, sp := newSatPipeline(1.0, 0.5)
		dc := newFakeDropCounter()
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		h.DropCounter = dc
		p.Start()
		w := httptest.NewRecorder()
		h.envelope(w, envelopeRequest(body))
		p.Close(context.Background())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if ev.count() != 0 {
			t.Errorf("батчер событий увидел %d, want 0 (насыщен)", ev.count())
		}
		if sp.count() != 1 {
			t.Errorf("SpanWriter увидел %d транзакций, want 1 (свободен)", sp.count())
		}
		if got := dc.events[1]; got != 1 {
			t.Errorf("IncDroppedEvents учёл %d, want 1 (насыщенное событие обязано считаться дропом)", got)
		}
	})

	t.Run("транзакции насыщены, события свободны", func(t *testing.T) {
		p, ev, sp := newSatPipeline(0.5, 1.0)
		dc := newFakeDropCounter()
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		h.DropCounter = dc
		p.Start()
		w := httptest.NewRecorder()
		h.envelope(w, envelopeRequest(body))
		p.Close(context.Background())

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		if ev.count() != 1 {
			t.Errorf("батчер событий увидел %d, want 1 (свободен)", ev.count())
		}
		if sp.count() != 0 {
			t.Errorf("SpanWriter увидел %d транзакций, want 0 (насыщен)", sp.count())
		}
		if got := dc.transactions[1]; got != 1 {
			t.Errorf("IncDroppedTransactions учёл %d, want 1 (насыщенная транзакция обязана считаться дропом)", got)
		}
	})
}

func TestOverloadEnvelopeAllSaturated(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem

	p, ev, sp := newSatPipeline(1.0, 1.0)
	q := &countingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)
	h.TxQuota = q
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0", ev.count(), sp.count())
	}
	if q.count() != 0 {
		t.Errorf("квота вызвана %d раз, want 0", q.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1 (событие присутствует — signal=event по правилу)", got)
	}
}

func TestOverloadEnvelopeOnlyEventSaturated(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, ev, _ := newSatPipeline(1.0, 0)
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0", ev.count())
	}
}

type denyingQuota struct{}

func (denyingQuota) CheckAndCount(context.Context, int64, int64) (int64, time.Time, error) {
	return 0, time.Time{}, nil
}
func (denyingQuota) Refund(context.Context, int64, int64, time.Time) error { return nil }

// один класс насыщен, другой честно исчерпал квоту — выигрывает переходная 503,
// не 429: тот сжёг бы Retry-After до конца месяца.
func TestOverloadEnvelopeOverloadBeatsQuota(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem

	t.Run("события насыщены, квота транзакций исчерпана", func(t *testing.T) {
		p, ev, sp := newSatPipeline(1.0, 0.5) // события насыщены, транзакции свободны
		dc := newFakeDropCounter()
		h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
		h.TxQuota = denyingQuota{}
		h.DropCounter = dc
		p.Start()
		w := httptest.NewRecorder()
		h.envelope(w, envelopeRequest(body))
		p.Close(context.Background())

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5 (переходная причина, не месячная)", got)
		}
		if ev.count() != 0 || sp.count() != 0 {
			t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0", ev.count(), sp.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
			t.Errorf("RejectedBy(overloaded, event) = %d, want 1 (насыщенный класс — причина ответа)", got)
		}
		if got := dc.events[1]; got != 0 {
			t.Errorf("IncDroppedEvents = %d, want 0: под 503 не принято ничего, дроп не считается "+
				"(насыщенный класс не потерян — клиент вернётся через 5с)", got)
		}
		if got := dc.transactions[1]; got != 0 {
			t.Errorf("IncDroppedTransactions = %d, want 0: квота не списана и не отражена дропом — "+
				"класс получит честный отказ заново при ретрае", got)
		}
	})

	t.Run("транзакции насыщены, квота событий исчерпана", func(t *testing.T) {
		p, ev, sp := newSatPipeline(0.5, 1.0) // события свободны, транзакции насыщены
		dc := newFakeDropCounter()
		h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
		h.DropCounter = dc
		p.Start()
		w := httptest.NewRecorder()
		h.envelope(w, envelopeRequest(body))
		p.Close(context.Background())

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if ev.count() != 0 || sp.count() != 0 {
			t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0", ev.count(), sp.count())
		}
		if got := h.RejectedBy(RejectOverloaded, SignalTransaction); got != 1 {
			t.Errorf("RejectedBy(overloaded, transaction) = %d, want 1 (насыщенный класс — причина ответа)", got)
		}
		if got := dc.events[1]; got != 0 {
			t.Errorf("IncDroppedEvents = %d, want 0", got)
		}
		if got := dc.transactions[1]; got != 0 {
			t.Errorf("IncDroppedTransactions = %d, want 0", got)
		}
	})
}

func TestOverloadEnvelopeBothQuotaExceededStaysOldBehavior(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem

	p, ev, sp := newSatPipeline(0, 0) // насыщения нет вовсе
	dc := newFakeDropCounter()
	h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
	h.TxQuota = denyingQuota{}
	h.DropCounter = dc
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (прежнее поведение — обе причины квотные)", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "5" {
		t.Errorf("Retry-After = %q, не должен быть переходным (5с) — это честная месячная квота", got)
	}
	if !strings.Contains(w.Body.String(), "event quota exceeded") {
		t.Errorf("тело = %s, want detail=event quota exceeded (событие есть — первым в правиле сигнала)", w.Body.String())
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0", ev.count(), sp.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 0 (насыщения не было — не overloaded)", got)
	}
	if got := dc.events[1]; got != 1 {
		t.Errorf("IncDroppedEvents = %d, want 1 (429 по чистой квоте дроп считает, как и раньше)", got)
	}
	if got := dc.transactions[1]; got != 1 {
		t.Errorf("IncDroppedTransactions = %d, want 1", got)
	}
}

const envelopeProfileItem = `{"type":"profile"}
{"platform":"python","transaction":{"name":"GET /x"},"profile":{"frames":[{"function":"main"},{"function":"slow"}],"stacks":[[1,0]],"samples":[{"stack_id":0},{"stack_id":0}]}}
`

func TestProfileAcceptedDespiteTransientOverloadQuotaClash(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem + envelopeProfileItem

	p, ev, sp := newSatPipeline(1.0, 0.5) // события насыщены, транзакции свободны
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 0}
	q := &fixedBudgetCountingQuota{n: 10} // квота профилей есть и должна быть использована
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.TxQuota = denyingQuota{}
	h.ProfileQuota = q
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (профиль принят, значит НЕ «ничего не принято»), body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0 (событие насыщено, транзакция без квоты)", ev.count(), sp.count())
	}
	if sink.count() != 1 {
		t.Errorf("ProfileSink увидел %d профилей, want 1 (у профиля своя здоровая квота и буфер)", sink.count())
	}
	if got := q.count(); got != 1 {
		t.Errorf("h.grant для профилей вызван %d раз, want 1", got)
	}
	if got := dc.profilesCalls; got != 0 {
		t.Errorf("IncDroppedProfiles вызван %d раз, want 0 (профиль принят, не дропнут)", got)
	}
}

func TestProfileTransitionalClashEventOverloadedProfileQuotaExhausted(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeProfileItem

	p, ev, _ := newSatPipeline(1.0, 0) // событие насыщено
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 0}
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.ProfileQuota = denyingQuota{}
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (переходная причина), body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1 (насыщенный класс — причина ответа)", got)
	}
	if ev.count() != 0 {
		t.Errorf("батчер событий увидел %d, want 0", ev.count())
	}
	if sink.count() != 0 {
		t.Errorf("ProfileSink увидел %d профилей, want 0 (под 503 не принято ничего)", sink.count())
	}
	if got := dc.profilesCalls; got != 0 {
		t.Errorf("IncDroppedProfiles вызван %d раз, want 0 (под 503 дроп не считается — честный шанс при ретрае)", got)
	}
}

func TestProfileTransitionalClashProfileOverloadedEventQuotaExhausted(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeProfileItem

	p, ev, _ := newSatPipeline(0, 0)
	sink := &satProfileSink{sat: 1.0} // буфер профилей насыщен
	h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (переходная причина), body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if got := h.RejectedBy(RejectOverloaded, SignalProfile); got != 1 {
		t.Errorf("RejectedBy(overloaded, profile) = %d, want 1 (насыщенный класс — причина ответа)", got)
	}
	if ev.count() != 0 {
		t.Errorf("батчер событий увидел %d, want 0", ev.count())
	}
	if sink.count() != 0 {
		t.Errorf("ProfileSink увидел %d профилей, want 0 (буфер насыщен)", sink.count())
	}
}

func TestProfilesSurviveEventAndTxQuotaExhaustion(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem + envelopeProfileItem

	p, ev, sp := newSatPipeline(0, 0) // насыщения буферов нет — обе причины отказа честно квотные
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 0}
	h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
	h.TxQuota = denyingQuota{}
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if sink.count() != 1 {
		t.Errorf("ProfileSink увидел %d профилей, want 1 (профиль не должен пропасть из-за чужой квоты)", sink.count())
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0 (обе квоты честно исчерпаны)", ev.count(), sp.count())
	}
	if got := dc.events[1]; got != 1 {
		t.Errorf("IncDroppedEvents = %d, want 1", got)
	}
	if got := dc.transactions[1]; got != 1 {
		t.Errorf("IncDroppedTransactions = %d, want 1", got)
	}
}

func TestProfilesDroppedWhenAllThreeQuotasExhausted(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeTxItem + envelopeProfileItem

	p, ev, sp := newSatPipeline(0, 0)
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 0}
	h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
	h.TxQuota = denyingQuota{}
	h.ProfileQuota = denyingQuota{}
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", w.Code, w.Body.String())
	}
	if sink.count() != 0 {
		t.Errorf("ProfileSink увидел %d профилей, want 0 (квота профилей тоже исчерпана)", sink.count())
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0", ev.count(), sp.count())
	}
	if got := dc.profilesCalls; got != 1 {
		t.Errorf("IncDroppedProfiles вызван %d раз, want 1 (дроп профиля обязан быть учтён)", got)
	}
}

func TestProfileOnlyEnvelopeQuotaExceededNowRejects(t *testing.T) {
	body := "{}\n" + envelopeProfileItem
	p, _, _ := newSatPipeline(0, 0)
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 0}
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.ProfileQuota = denyingQuota{}
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (новое поведение T4), body=%s", w.Code, w.Body.String())
	}
	if sink.count() != 0 {
		t.Errorf("ProfileSink увидел %d профилей, want 0", sink.count())
	}
	if got := dc.profilesCalls; got != 1 {
		t.Errorf("IncDroppedProfiles вызван %d раз, want 1", got)
	}
	if got := h.RejectedBy(RejectQuota, SignalProfile); got != 1 {
		t.Errorf("RejectedBy(quota, profile) = %d, want 1", got)
	}
}

func TestProfileOnlyEnvelopeQuotaAvailableAccepted(t *testing.T) {
	body := "{}\n" + envelopeProfileItem
	p, _, _ := newSatPipeline(0, 0)
	sink := &satProfileSink{sat: 0}
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if sink.count() != 1 {
		t.Errorf("ProfileSink увидел %d профилей, want 1", sink.count())
	}
}

func TestProfileOverloadedBufferIgnoresQuotaRule(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
` + envelopeProfileItem

	p, ev, _ := newSatPipeline(0, 0)
	dc := newFakeDropCounter()
	sink := &satProfileSink{sat: 1.0} // буфер профилей насыщен, квоты ни при чём
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.DropCounter = dc
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (событие свободно, насыщение профиля не должно давать 429/503), body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 1 {
		t.Errorf("батчер событий увидел %d, want 1", ev.count())
	}
	if sink.count() != 0 {
		t.Errorf("ProfileSink увидел %d профилей, want 0 (буфер насыщен)", sink.count())
	}
	if h.ProfileQuota != nil {
		t.Fatalf("тест предполагает h.ProfileQuota == nil (грант не должен зваться при насыщении)")
	}
	if got := dc.profilesCalls; got != 1 {
		t.Errorf("IncDroppedProfiles вызван %d раз, want 1", got)
	}
}

// как fixedQuotaChecker, но вдобавок считает обращения — важно не только
// сколько выдано, но и сколько раз квоту вообще спрашивали.
type fixedBudgetCountingQuota struct {
	mu    sync.Mutex
	n     int64
	calls int
}

func (q *fixedBudgetCountingQuota) CheckAndCount(_ context.Context, _ int64, want int64) (int64, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls++
	granted := want
	if granted > q.n {
		granted = q.n
	}
	q.n -= granted
	return granted, time.Time{}, nil
}

// профили вытесняются, а не отклоняются постановкой — возврат по ёмкости не участвует.
func (q *fixedBudgetCountingQuota) Refund(context.Context, int64, int64, time.Time) error { return nil }

func (q *fixedBudgetCountingQuota) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

// h.grant для профилей обязан вызываться ровно один раз на запрос — лишний
// вызов значит, что клиент платит за один профиль дважды.
func TestProfileQuotaGrantedExactlyOnce(t *testing.T) {
	body := "{}\n" + envelopeProfileItem
	p, _, _ := newSatPipeline(0, 0)
	sink := &satProfileSink{sat: 0}
	q := &fixedBudgetCountingQuota{n: 10}
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.ProfileQuota = q
	h.Profiles = sink
	p.Start()
	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))
	p.Close(context.Background())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if sink.count() != 1 {
		t.Errorf("ProfileSink увидел %d профилей, want 1", sink.count())
	}
	if got := q.count(); got != 1 {
		t.Errorf("h.grant для профилей вызван %d раз, want 1 (двойное списание квоты)", got)
	}
}
