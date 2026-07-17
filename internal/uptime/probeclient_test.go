package uptime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// fakeCenter — httptest-«центр» для юнит-тестов клиента пробы: раздаёт
// заранее подготовленные задания на /probe/lease и копит присланные пачки
// результатов с /probe/results. Никакой БД — только сетевой протокол.
type fakeCenter struct {
	mu sync.Mutex

	jobs      []uptime.JobDTO // выдаётся ОДИН раз, дальше lease отдаёт пусто
	handedOut bool
	leaseCode int // 0 = 200; иначе этим кодом отвечаем на /probe/lease

	leaseCalls    int
	leaseAuth     []string
	resultsAuth   []string
	resultBatches [][]uptime.ResultDTO
}

func (c *fakeCenter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /probe/lease", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.leaseCalls++
		c.leaseAuth = append(c.leaseAuth, r.Header.Get("Authorization"))
		code := c.leaseCode
		var jobs []uptime.JobDTO
		if !c.handedOut {
			jobs = c.jobs
			c.handedOut = true
		}
		c.mu.Unlock()

		if code != 0 {
			w.WriteHeader(code)
			return
		}
		writeJSON(w, uptime.LeaseResponse{ProbeID: 7, Region: "eu-west", Jobs: jobs})
	})
	mux.HandleFunc("POST /probe/results", func(w http.ResponseWriter, r *http.Request) {
		var req uptime.ResultsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.resultsAuth = append(c.resultsAuth, r.Header.Get("Authorization"))
		c.resultBatches = append(c.resultBatches, req.Results)
		c.mu.Unlock()
		writeJSON(w, uptime.ResultsResponse{Accepted: len(req.Results)})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (c *fakeCenter) snapshot() (leaseCalls int, batches [][]uptime.ResultDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batches = make([][]uptime.ResultDTO, len(c.resultBatches))
	copy(batches, c.resultBatches)
	return c.leaseCalls, batches
}

// waitFor polls cond until true or 5s pass — the client's loop is async.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in 5s")
}

// runClient запускает клиента горутиной и гарантирует его остановку к концу
// теста.
func runClient(t *testing.T, c *uptime.ProbeClient) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("ProbeClient.Run did not return after ctx cancel")
		}
	})
}

// stubChecker — чекер-заглушка (Result без сети), как Checkers-подмена у Runner.
type stubChecker struct {
	res    uptime.Result
	panics bool

	mu    sync.Mutex
	calls int
}

func (s *stubChecker) Check(_ context.Context, _ uptime.Monitor) uptime.Result {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.panics {
		panic("boom")
	}
	return s.res
}

func (s *stubChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func httpJob(t *testing.T, queueID, monitorID int64, url string) uptime.JobDTO {
	t.Helper()
	cfg, err := json.Marshal(uptime.HTTPConfig{Method: "GET", URL: url})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return uptime.JobDTO{
		QueueID:        queueID,
		MonitorID:      monitorID,
		Kind:           uptime.KindHTTP,
		Config:         cfg,
		TimeoutSeconds: 5,
	}
}

func TestProbeClientRunsJobAndPostsResult(t *testing.T) {
	// Спим несколько миллисекунд: тайминги считаются в целых мс, а ответ по
	// loopback приходит быстрее, чем за 1 мс, — иначе Total честно был бы 0 и
	// проверка "тайминги проехали до центра" ничего бы не проверяла.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	center := &fakeCenter{jobs: []uptime.JobDTO{httpJob(t, 42, 3, target.URL)}}
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	client := &uptime.ProbeClient{
		ServerURL: srv.URL,
		Token:     "secret-token",
		PollEvery: 10 * time.Millisecond,
		// Цель проверки — loopback-сервер httptest: отключаем SSRF-фильтр,
		// иначе проверка резалась бы до соединения.
		AllowPrivateTargets: true,
	}
	runClient(t, client)

	waitFor(t, func() bool {
		_, batches := center.snapshot()
		return len(batches) > 0
	})

	_, batches := center.snapshot()
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("result batches = %v, want one batch with one result", batches)
	}
	got := batches[0][0]
	if got.QueueID != 42 {
		t.Errorf("QueueID = %d, want 42", got.QueueID)
	}
	if !got.OK {
		t.Errorf("OK = false, want true (error %q)", got.Error)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	if got.Timings.Total == 0 {
		t.Errorf("Timings.Total = 0, want > 0 (timings %+v)", got.Timings)
	}

	center.mu.Lock()
	defer center.mu.Unlock()
	if len(center.leaseAuth) == 0 || center.leaseAuth[0] != "Bearer secret-token" {
		t.Errorf("lease Authorization = %v, want Bearer secret-token", center.leaseAuth)
	}
	if len(center.resultsAuth) == 0 || center.resultsAuth[0] != "Bearer secret-token" {
		t.Errorf("results Authorization = %v, want Bearer secret-token", center.resultsAuth)
	}
}

func TestProbeClientPostsNothingWhenNoJobs(t *testing.T) {
	center := &fakeCenter{} // заданий нет вообще
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	client := &uptime.ProbeClient{
		ServerURL: srv.URL,
		Token:     "t",
		PollEvery: 5 * time.Millisecond,
	}
	runClient(t, client)

	waitFor(t, func() bool {
		calls, _ := center.snapshot()
		return calls >= 3
	})
	if _, batches := center.snapshot(); len(batches) != 0 {
		t.Fatalf("result batches = %v, want none (no jobs leased)", batches)
	}
}

func TestProbeClientSurvivesLeaseServerError(t *testing.T) {
	center := &fakeCenter{leaseCode: http.StatusInternalServerError}
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	client := &uptime.ProbeClient{
		ServerURL: srv.URL,
		Token:     "t",
		PollEvery: 5 * time.Millisecond,
	}
	runClient(t, client)

	// Тик, упавший на lease, пропускается — но цикл живёт и стучится снова.
	waitFor(t, func() bool {
		calls, _ := center.snapshot()
		return calls >= 3
	})
	if _, batches := center.snapshot(); len(batches) != 0 {
		t.Fatalf("result batches = %v, want none", batches)
	}
}

func TestProbeClientSplitsResultsIntoBatchesOf100(t *testing.T) {
	jobs := make([]uptime.JobDTO, 150)
	for i := range jobs {
		jobs[i] = uptime.JobDTO{
			QueueID:        int64(i + 1),
			MonitorID:      int64(i + 1),
			Kind:           uptime.KindHTTP,
			Config:         json.RawMessage(`{"method":"GET","url":"http://example.invalid"}`),
			TimeoutSeconds: 5,
		}
	}
	center := &fakeCenter{jobs: jobs}
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	checker := &stubChecker{res: uptime.Result{OK: true, StatusCode: 200, TotalMs: 1}}
	client := &uptime.ProbeClient{
		ServerURL:   srv.URL,
		Token:       "t",
		PollEvery:   5 * time.Millisecond,
		Concurrency: 10,
		Checkers:    map[uptime.Kind]uptime.Checker{uptime.KindHTTP: checker},
	}
	runClient(t, client)

	waitFor(t, func() bool {
		_, batches := center.snapshot()
		total := 0
		for _, b := range batches {
			total += len(b)
		}
		return total >= 150
	})

	_, batches := center.snapshot()
	if len(batches) != 2 {
		t.Fatalf("got %d result requests, want 2 (150 results, batch cap 100)", len(batches))
	}
	seen := map[int64]bool{}
	for _, b := range batches {
		if len(b) > 100 {
			t.Errorf("batch of %d results, want <= 100", len(b))
		}
		for _, r := range b {
			seen[r.QueueID] = true
		}
	}
	if len(seen) != 150 {
		t.Errorf("got %d distinct queue_ids, want 150", len(seen))
	}
	if checker.callCount() != 150 {
		t.Errorf("checker calls = %d, want 150", checker.callCount())
	}
}

func TestProbeClientReportsCheckerPanicAsFailedResult(t *testing.T) {
	center := &fakeCenter{jobs: []uptime.JobDTO{{
		QueueID: 9, MonitorID: 1, Kind: uptime.KindHTTP,
		Config: json.RawMessage(`{"method":"GET","url":"http://example.invalid"}`), TimeoutSeconds: 5,
	}}}
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	client := &uptime.ProbeClient{
		ServerURL: srv.URL,
		Token:     "t",
		PollEvery: 5 * time.Millisecond,
		Checkers:  map[uptime.Kind]uptime.Checker{uptime.KindHTTP: &stubChecker{panics: true}},
	}
	runClient(t, client)

	waitFor(t, func() bool {
		_, batches := center.snapshot()
		return len(batches) > 0
	})

	_, batches := center.snapshot()
	got := batches[0][0]
	if got.OK || got.Error != "internal checker panic" {
		t.Fatalf("result = %+v, want OK=false Error=%q", got, "internal checker panic")
	}
}

func TestProbeClientRunReturnsOnContextCancel(t *testing.T) {
	center := &fakeCenter{}
	srv := httptest.NewServer(center.handler())
	defer srv.Close()

	client := &uptime.ProbeClient{ServerURL: srv.URL, Token: "t", PollEvery: 5 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()

	waitFor(t, func() bool {
		calls, _ := center.snapshot()
		return calls >= 1
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s after ctx cancel")
	}
}
