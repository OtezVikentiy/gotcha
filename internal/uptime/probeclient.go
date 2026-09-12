package uptime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultPollEvery        = time.Second
	defaultProbeHTTPTimeout = 30 * time.Second
	// потолок пачки /probe/results на стороне центра — больше он не примет,
	// клиент режет сам.
	maxResultsPerBatch = 100
)

type ProbeClient struct {
	ServerURL string // база центра, например https://gotcha.example.com
	Token     string // GOTCHA_PROBE_KEY; в логи не попадает никогда

	Concurrency int           // одновременных проверок; 0 = defaultConcurrency
	PollEvery   time.Duration // период опроса центра; 0 = defaultPollEvery

	AllowPrivateTargets bool // false (по умолчанию) — SSRF-фильтр приватных целей включён

	HTTPClient *http.Client // клиент для походов В ЦЕНТР, не для самих проверок; nil — таймаут 30s

	Checkers map[Kind]Checker // переопределение CheckerFor по Kind для тестов; nil — пакетный

	defaultClientOnce sync.Once    // ленивая сборка дефолтного клиента (см. httpClient)
	defaultClient     *http.Client // используется, только когда HTTPClient == nil
}

func (c *ProbeClient) concurrency() int {
	if c.Concurrency <= 0 {
		return defaultConcurrency
	}
	return c.Concurrency
}

func (c *ProbeClient) pollEvery() time.Duration {
	if c.PollEvery <= 0 {
		return defaultPollEvery
	}
	return c.PollEvery
}

// дефолтный клиент собирается ровно один раз — новый http.Client на каждый
// тик был бы лишней аллокацией на горячем пути.
func (c *ProbeClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.defaultClientOnce.Do(func() {
		c.defaultClient = &http.Client{Timeout: defaultProbeHTTPTimeout}
	})
	return c.defaultClient
}

func (c *ProbeClient) checkerFor(kind Kind) (Checker, error) {
	if ch, ok := c.Checkers[kind]; ok {
		return ch, nil
	}
	return CheckerFor(kind, c.AllowPrivateTargets)
}

// ошибка сети/5xx на lease или results — лог и пропуск тика целиком: проба не
// копит состояние, центр вернёт незавершённые задания в очередь по lease.
func (c *ProbeClient) Run(ctx context.Context) {
	tick := time.NewTicker(c.pollEvery())
	defer tick.Stop()

	slog.Info("probe client started", "server_url", c.ServerURL, "concurrency", c.concurrency())

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.tick(ctx)
		}
	}
}

func (c *ProbeClient) tick(ctx context.Context) {
	jobs, err := c.lease(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // штатная остановка, не ошибка
		}
		slog.Error("uptime: probe: lease failed", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	results := c.runJobs(ctx, jobs)
	if ctx.Err() != nil {
		return // ctx отменён прямо во время проверок — результаты уже некому слать
	}

	for chunk := range slices.Chunk(results, maxResultsPerBatch) {
		if err := c.postResults(ctx, chunk); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("uptime: probe: post results failed", "count", len(chunk), "error", err)
			// дальше пробовать нет смысла — если центр недоступен, недоступен и
			// для следующей пачки; оставшиеся задания вернутся по lease.
			return
		}
	}
}

// возвращается, только когда отработали все запущенные проверки — иначе
// результат мог бы приехать после отправки пачки.
func (c *ProbeClient) runJobs(ctx context.Context, jobs []JobDTO) []ResultDTO {
	sem := make(chan struct{}, c.concurrency())

	var (
		mu      sync.Mutex
		results = make([]ResultDTO, 0, len(jobs))
		wg      sync.WaitGroup
	)

	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// нераспределённые задания останутся за пробой до истечения lease
			// и приедут снова — как и любой другой пропущенный тик.
			wg.Wait()
			return results
		}

		wg.Add(1)
		go func(j JobDTO) {
			defer wg.Done()
			defer func() { <-sem }()

			res, ok := c.runOne(ctx, j)
			if !ok {
				return
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(j)
	}

	wg.Wait()
	return results
}

// паника чекера превращается в Result{OK:false, Error:"internal checker
// panic"}; ok=false — чекера под этот kind нет, задание вернётся по lease.
func (c *ProbeClient) runOne(ctx context.Context, j JobDTO) (ResultDTO, bool) {
	checker, err := c.checkerFor(j.Kind)
	if err != nil {
		slog.Error("uptime: probe: no checker for job", "monitor_id", j.MonitorID, "kind", j.Kind, "error", err)
		return ResultDTO{}, false
	}

	m := j.Monitor()
	result := func() (res Result) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("uptime: probe: checker panicked", "monitor_id", j.MonitorID, "kind", j.Kind, "panic", p)
				res = Result{OK: false, Error: "internal checker panic"}
			}
		}()
		return checkWithRetries(ctx, checker, m)
	}()

	return NewResultDTO(j.QueueID, result), true
}

func (c *ProbeClient) lease(ctx context.Context) ([]JobDTO, error) {
	var resp LeaseResponse
	if err := c.post(ctx, "/probe/lease", LeaseRequest{Limit: c.concurrency()}, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

func (c *ProbeClient) postResults(ctx context.Context, results []ResultDTO) error {
	var resp ResultsResponse
	if err := c.post(ctx, "/probe/results", ResultsRequest{Results: results}, &resp); err != nil {
		return err
	}
	if resp.Rejected > 0 {
		// норма, не сбой — lease истёк или задание уже выполнено другой пробой.
		slog.Warn("uptime: probe: results rejected by server", "accepted", resp.Accepted, "rejected", resp.Rejected)
	}
	return nil
}

func (c *ProbeClient) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("uptime: probe: encode %s request: %w", path, err)
	}

	url := strings.TrimSuffix(c.ServerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("uptime: probe: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("uptime: probe: %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// тело не логируем целиком — доверять его размеру незачем.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("uptime: probe: %s: unexpected status %d", path, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("uptime: probe: decode %s response: %w", path, err)
	}
	return nil
}
