package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
)

// newTestRunner — runner собранный напрямую из внутренностей пакета (не через
// Run): тесты подставляют свои collector/sender/buffer и вызывают tick сами,
// с любыми значениями now — интервал Config (границы 10s..5m) тут ни при чём.
// rng — фиксированный seed: тесты воспроизводимы, но джиттер бэкоффа реально
// участвует (в отличие от nil-rng, который jitterBackoff тихо занулил бы).
func newTestRunner(t *testing.T, endpoint string) *runner {
	t.Helper()
	sender, err := NewSender(Config{Endpoint: endpoint, Key: "test-key"})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return &runner{
		hostname:  "test-host",
		collector: NewCollector(fakeProbes()),
		sender:    sender,
		buffer:    NewBuffer(bufferMaxBatches, bufferMaxBytes),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		rng:       rand.New(rand.NewSource(42)),
	}
}

// newTestRunnerWithLog — как newTestRunner, но лог пишется в буфер, который
// тест может проверить (число строк, наличие маркеров) — для тестов
// логирования переходов состояния (задача 2 волны R-C).
func newTestRunnerWithLog(t *testing.T, endpoint string) (*runner, *bytes.Buffer) {
	t.Helper()
	sender, err := NewSender(Config{Endpoint: endpoint, Key: "test-key"})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	var logBuf bytes.Buffer
	r := &runner{
		hostname:  "test-host",
		collector: NewCollector(fakeProbes()),
		sender:    sender,
		buffer:    NewBuffer(bufferMaxBatches, bufferMaxBytes),
		log:       slog.New(slog.NewTextHandler(&logBuf, nil)),
		rng:       rand.New(rand.NewSource(42)),
	}
	return r, &logBuf
}

// decodeExport распаковывает gzip-protobuf тело запроса (Sender.Send всегда
// шлёт Content-Encoding: gzip, см. sender.go).
func decodeExport(t *testing.T, r *http.Request) *metricspb.MetricsData {
	t.Helper()
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	md := &metricspb.MetricsData{}
	if err := proto.Unmarshal(raw, md); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return md
}

func resourceAttr(md *metricspb.MetricsData, key string) string {
	for _, rm := range md.GetResourceMetrics() {
		for _, kv := range rm.GetResource().GetAttributes() {
			if kv.GetKey() == key {
				return kv.GetValue().GetStringValue()
			}
		}
	}
	return ""
}

// dataPointTime достаёт TimeUnixNano первой точки system.cpu.logical.count —
// эта метрика эмитится безусловно на любом тике (в отличие от CPU-gauge,
// которого нет на первом тике коллектора), удобный якорь для сверки таймстемпа
// экспорта с исходным тиком.
func dataPointTime(t *testing.T, md *metricspb.MetricsData) time.Time {
	t.Helper()
	for _, rm := range md.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() != hostmetric.CPULogicalCount {
					continue
				}
				dps := m.GetSum().GetDataPoints()
				if len(dps) == 0 {
					t.Fatal("system.cpu.logical.count без датапойнтов")
				}
				return time.Unix(0, int64(dps[0].GetTimeUnixNano()))
			}
		}
	}
	t.Fatal("system.cpu.logical.count не найдена в экспорте")
	return time.Time{}
}

// TestRunnerTickSendsBatch: тик собирает Sample, шлёт его на сервер одним
// экспортом с host.name из cfg.Hostname (здесь — прямо из runner.hostname).
func TestRunnerTickSendsBatch(t *testing.T) {
	var mu sync.Mutex
	var got *metricspb.MetricsData
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md := decodeExport(t, r)
		mu.Lock()
		got = md
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.tick(context.Background(), time.Unix(1000, 0))

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("сервер не получил экспорт")
	}
	if host := resourceAttr(got, "host.name"); host != "test-host" {
		t.Errorf("host.name = %q, хочу test-host", host)
	}
}

// TestRunFirstTickImmediate: Run отправляет первый батч сразу при старте, не
// дожидаясь истечения cfg.Interval (задача 14) — иначе свежеустановленный
// агент молчит на карточке хоста вплоть до максимального интервала (5 минут).
// Interval взят максимальным (maxInterval), чтобы тест провалился, если
// правку когда-нибудь откатят к «ждём первого тика ticker'а».
func TestRunFirstTickImmediate(t *testing.T) {
	reqCh := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reqCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{Endpoint: srv.URL, Key: "test-key", Hostname: "test-host", Interval: maxInterval}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	select {
	case <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не отправил первый батч сразу — похоже, ждёт полного cfg.Interval перед первым тиком")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run вернул ошибку: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены ctx")
	}
}

// TestRunnerBuffersOnOutage: сервер отвечает 500 три тика подряд — все три
// батча копятся в буфере (buffer.Len()==3). Когда сервер оживает, следующий
// тик доставляет ТЕКУЩИЙ батч и следом дренирует буфер oldest-first — итого
// 4 экспорта за этот тик, и буферные экспорты несут исходные таймстемпы своих
// тиков (не таймстемп момента дренажа).
func TestRunnerBuffersOnOutage(t *testing.T) {
	var mu sync.Mutex
	var reqs []*metricspb.MetricsData
	var healthy int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md := decodeExport(t, r)
		mu.Lock()
		reqs = append(reqs, md)
		mu.Unlock()
		if atomic.LoadInt32(&healthy) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	base := time.Unix(1_000_000, 0)
	// запас над максимальным бэкоффом (10м), чтобы каждый тик outage реально
	// доходил до сервера, а не резался полом notBefore предыдущего отказа.
	const spacing = 11 * time.Minute
	tickTimes := []time.Time{base, base.Add(spacing), base.Add(2 * spacing)}
	for _, tt := range tickTimes {
		r.tick(context.Background(), tt)
	}
	if got := r.buffer.Len(); got != 3 {
		t.Fatalf("buffer.Len() после трёх отказов = %d, хочу 3", got)
	}

	atomic.StoreInt32(&healthy, 1)
	mu.Lock()
	reqs = nil // дальше считаем только запросы «оживающего» тика
	mu.Unlock()
	revival := base.Add(3 * spacing)
	r.tick(context.Background(), revival)

	if got := r.buffer.Len(); got != 0 {
		t.Fatalf("buffer.Len() после дренажа = %d, хочу 0", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 4 {
		t.Fatalf("экспортов на ожившем тике = %d, хочу 4 (текущий + 3 дренаж)", len(reqs))
	}
	wantTS := []time.Time{revival, tickTimes[0], tickTimes[1], tickTimes[2]}
	for i, md := range reqs {
		got := dataPointTime(t, md)
		if !got.Equal(wantTS[i]) {
			t.Errorf("reqs[%d] таймстемп = %v, хочу %v", i, got, wantTS[i])
		}
	}
}

// TestRunnerDrainFailureBacksOff: текущий батч уходит успешно (200), но
// дренаж предзаполненного буфера ловит 500 — батч дренажа ОСТАЁТСЯ в буфере
// (Oldest без DropOldest), fails/notBefore обновляются. Следующий тик внутри
// пола не должен снова долбить сервер дренажом.
func TestRunnerDrainFailureBacksOff(t *testing.T) {
	var reqN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqN, 1)
		if n == 1 {
			w.WriteHeader(http.StatusOK) // текущий батч тика
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // дренаж
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.buffer.Push([]byte("stale-batch")) // предзаполненный буфер — содержимое серверу не важно

	base := time.Unix(1000, 0)
	r.tick(context.Background(), base)

	if got := atomic.LoadInt32(&reqN); got != 2 {
		t.Fatalf("запросов = %d, хочу 2 (текущий + дренаж)", got)
	}
	if got := r.buffer.Len(); got != 1 {
		t.Fatalf("buffer.Len() = %d, хочу 1 (батч дренажа остался)", got)
	}
	if r.fails != 1 {
		t.Fatalf("fails = %d, хочу 1", r.fails)
	}
	if !r.notBefore.After(base) {
		t.Fatalf("notBefore не сдвинут вперёд: %v", r.notBefore)
	}

	// тик внутри пола: дренаж не должен пытаться снова долбить сервер.
	r.tick(context.Background(), base.Add(time.Second))
	if got := atomic.LoadInt32(&reqN); got != 2 {
		t.Fatalf("запросов после тика внутри пола = %d, хочу 2 (новых быть не должно)", got)
	}
	if got := r.buffer.Len(); got != 2 {
		t.Fatalf("buffer.Len() = %d, хочу 2 (текущий тик добавился в буфер без попытки дренажа)", got)
	}
}

// TestRunnerDropsOn401: отозванный ключ — повтор не поможет, батч не
// буферизуется вовсе.
func TestRunnerDropsOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	base := time.Unix(1000, 0)
	r.tick(context.Background(), base)
	r.tick(context.Background(), base.Add(20*time.Second))

	if got := r.buffer.Len(); got != 0 {
		t.Fatalf("buffer.Len() = %d, хочу 0 (401 — Drop, не буферизуем)", got)
	}
}

// TestRunnerBackoffFloor: 429 с Retry-After: 3600 задаёт часовой пол — до его
// истечения ни один следующий тик не должен доходить до сервера вообще (сразу
// буферизуется без попытки Send).
func TestRunnerBackoffFloor(t *testing.T) {
	var reqN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqN, 1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	base := time.Unix(1000, 0)
	r.tick(context.Background(), base)

	if got := atomic.LoadInt32(&reqN); got != 1 {
		t.Fatalf("запросов после первого тика = %d, хочу 1", got)
	}
	if got := r.buffer.Len(); got != 1 {
		t.Fatalf("buffer.Len() = %d, хочу 1", got)
	}

	r.tick(context.Background(), base.Add(30*time.Second))
	if got := atomic.LoadInt32(&reqN); got != 1 {
		t.Fatalf("запросов после второго тика = %d, хочу по-прежнему 1 (пол ещё не истёк)", got)
	}
	if got := r.buffer.Len(); got != 2 {
		t.Fatalf("buffer.Len() = %d, хочу 2 (второй тик тоже буферизован без попытки отправки)", got)
	}
}

// TestRunnerDrainCapsPerTick: буфер с 20 батчами и здоровым сервером —
// дренаж за один тик выгружает не больше maxDrainPerTick, остаток уходит
// следующими тиками (ops-MED, thundering herd — см. комментарий у
// maxDrainPerTick в run.go). Текущий батч тика при этом отправляется всегда.
func TestRunnerDrainCapsPerTick(t *testing.T) {
	var reqN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqN, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	const prefilled = 20
	for i := 0; i < prefilled; i++ {
		r.buffer.Push([]byte("stale-batch"))
	}

	base := time.Unix(1000, 0)
	r.tick(context.Background(), base)

	if got := atomic.LoadInt32(&reqN); got != 1+maxDrainPerTick {
		t.Fatalf("запросов за тик = %d, хочу %d (текущий + кап дренажа)", got, 1+maxDrainPerTick)
	}
	if got := r.buffer.Len(); got != prefilled-maxDrainPerTick {
		t.Fatalf("buffer.Len() после тика = %d, хочу %d (кап дренажа не вычистил всё разом)", got, prefilled-maxDrainPerTick)
	}

	// Сервер по-прежнему здоров — следующий тик дренирует ещё порцию.
	r.tick(context.Background(), base.Add(time.Second))
	if got := r.buffer.Len(); got != prefilled-2*maxDrainPerTick {
		t.Fatalf("buffer.Len() после второго тика = %d, хочу %d", got, prefilled-2*maxDrainPerTick)
	}
}

// TestRunnerFirstDeliveryLoggedOnce: строка "first batch delivered" должна
// появиться в журнале ровно один раз за жизнь runner'а, а не на каждом
// здоровом тике (ops-MED, немота).
func TestRunnerFirstDeliveryLoggedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, logBuf := newTestRunnerWithLog(t, srv.URL)
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		r.tick(context.Background(), base.Add(time.Duration(i)*time.Second))
	}

	if got := strings.Count(logBuf.String(), "first batch delivered"); got != 1 {
		t.Fatalf("вхождений \"first batch delivered\" в логе = %d, хочу 1 (не растёт на каждом здоровом тике)", got)
	}
}

// TestRunnerBufferingAndRecoveryLoggedOnTransition: переход в буферизацию и
// восстановление логируются один раз на смену состояния, а не на каждой
// неудаче/каждом здоровом тике (ops-MED, немота).
func TestRunnerBufferingAndRecoveryLoggedOnTransition(t *testing.T) {
	var healthy int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, logBuf := newTestRunnerWithLog(t, srv.URL)
	base := time.Unix(1_000_000, 0)
	const spacing = 11 * time.Minute // запас над max(backoffFor)+jitter, как в TestRunnerBuffersOnOutage

	r.tick(context.Background(), base)
	r.tick(context.Background(), base.Add(spacing))

	if got := strings.Count(logBuf.String(), "entering buffered mode"); got != 1 {
		t.Fatalf("вхождений \"entering buffered mode\" = %d, хочу 1", got)
	}
	if strings.Contains(logBuf.String(), "delivery recovered") {
		t.Fatal("\"delivery recovered\" не должно появляться до восстановления связи")
	}

	atomic.StoreInt32(&healthy, 1)
	r.tick(context.Background(), base.Add(2*spacing))

	if got := strings.Count(logBuf.String(), "delivery recovered"); got != 1 {
		t.Fatalf("вхождений \"delivery recovered\" = %d, хочу 1", got)
	}
	if got := strings.Count(logBuf.String(), "entering buffered mode"); got != 1 {
		t.Fatalf("вхождений \"entering buffered mode\" после восстановления = %d, хочу по-прежнему 1 (не задвоилось)", got)
	}
}

// TestRunnerBackoffAfterFailureNotBeforeBase: джиттер только добавляет
// ожидание — notBefore после backoffAfterFailure не может оказаться раньше
// базового бэкоффа (без джиттера) и не превышает базовый бэкофф больше, чем
// на потолок джиттера (min(wait/4, backoffCap)).
func TestRunnerBackoffAfterFailureNotBeforeBase(t *testing.T) {
	r := newTestRunner(t, "http://unused.invalid")
	base := time.Unix(1000, 0)
	r.backoffAfterFailure(base, 0)

	baseWait := backoffFor(1)
	minNotBefore := base.Add(baseWait)
	if r.notBefore.Before(minNotBefore) {
		t.Fatalf("notBefore = %v раньше базового бэкоффа %v — джиттер ускорил ретрай", r.notBefore, minNotBefore)
	}
	maxJitter := baseWait / 4
	if maxJitter > backoffCap {
		maxJitter = backoffCap
	}
	if maxNotBefore := minNotBefore.Add(maxJitter); r.notBefore.After(maxNotBefore) {
		t.Fatalf("notBefore = %v превышает разумный потолок %v", r.notBefore, maxNotBefore)
	}
}

// TestJitterBackoffBounds: джиттер всегда в [0, min(wait/4, backoffCap)] —
// проверено по множеству вызовов одного rng, чтобы не зависеть от конкретного
// исхода одного семпла.
func TestJitterBackoffBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	wait := 100 * time.Second
	maxJitter := wait / 4
	for i := 0; i < 1000; i++ {
		j := jitterBackoff(rng, wait)
		if j < 0 {
			t.Fatalf("jitterBackoff вернул отрицательное значение: %v", j)
		}
		if j > maxJitter {
			t.Fatalf("jitterBackoff = %v превышает потолок %v", j, maxJitter)
		}
	}
}

// TestJitterBackoffCappedAtBackoffCap: даже при часовом wait (имитация floor
// из Retry-After) джиттер не превышает backoffCap.
func TestJitterBackoffCappedAtBackoffCap(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	wait := time.Hour
	for i := 0; i < 200; i++ {
		if j := jitterBackoff(rng, wait); j > backoffCap {
			t.Fatalf("jitterBackoff = %v превышает backoffCap %v даже при часовом wait", j, backoffCap)
		}
	}
}

// TestJitterBackoffDeterministicWithSeed: одинаковый seed даёт одинаковую
// последовательность джиттера — воспроизводимость для тестов и для
// расхождения фаз по хостам (seedFromHost).
func TestJitterBackoffDeterministicWithSeed(t *testing.T) {
	wait := 5 * time.Minute
	rng1 := rand.New(rand.NewSource(123))
	rng2 := rand.New(rand.NewSource(123))
	for i := 0; i < 10; i++ {
		a := jitterBackoff(rng1, wait)
		b := jitterBackoff(rng2, wait)
		if a != b {
			t.Fatalf("одинаковый seed дал разный джиттер на шаге %d: %v != %v", i, a, b)
		}
	}
}

// TestJitterBackoffZeroWait: нулевой/nil-rng вход не паникует и не даёт
// джиттера — nil.rng бывает у runner'ов, собранных напрямую без seed (edge
// case конструктора).
func TestJitterBackoffZeroWait(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	if j := jitterBackoff(rng, 0); j != 0 {
		t.Fatalf("jitterBackoff(rng, 0) = %v, хочу 0", j)
	}
	if j := jitterBackoff(nil, time.Minute); j != 0 {
		t.Fatalf("jitterBackoff(nil, ...) = %v, хочу 0 (не паникует, не даёт джиттера)", j)
	}
}

// TestSeedFromHostVariesByHostAndPID: разные хосты/PID дают разные seed —
// иначе весь парк с одинаковым PID (типично для контейнеров, PID 1) сходился
// бы по фазе джиттера обратно к thundering herd.
func TestSeedFromHostVariesByHostAndPID(t *testing.T) {
	a := seedFromHost("host-a", 1)
	b := seedFromHost("host-b", 1)
	if a == b {
		t.Fatal("seedFromHost одинаков для разных hostname при одном PID")
	}
	c := seedFromHost("host-a", 2)
	if a == c {
		t.Fatal("seedFromHost одинаков для разных PID при одном hostname")
	}
}
