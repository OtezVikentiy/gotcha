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
	// maxResultsPerBatch — потолок пачки /probe/results на стороне центра
	// (спека §4): больше он не примет, поэтому клиент режет сам.
	maxResultsPerBatch = 100
)

// ProbeClient — выносная проба: тот же бинарник в --mode=probe, запущенный в
// другом регионе. Из инфраструктуры ей нужен только исходящий HTTPS до центра
// — ни БД, ни ClickHouse, ни входящих портов. Проба «тупая»: она забирает
// задания (POST /probe/lease), гоняет обычные чекеры (те же, что и локальный
// Runner) и отдаёт сырые результаты (POST /probe/results). Ни детекции, ни
// состояния, ни локального накопления: всё это делает центр.
//
// Zero-value полей (Concurrency/PollEvery/HTTPClient/Checkers) означает
// "используй дефолт" — ProbeClient, как Runner, собирается литералом без
// конструктора.
type ProbeClient struct {
	ServerURL string // база центра, например https://gotcha.example.com
	Token     string // GOTCHA_PROBE_TOKEN; в логи не попадает никогда

	Concurrency int           // одновременных проверок; 0 = defaultConcurrency
	PollEvery   time.Duration // период опроса центра; 0 = defaultPollEvery

	// HTTPClient — клиент для походов В ЦЕНТР (не для самих проверок, у
	// чекеров свои). nil — клиент с таймаутом 30s.
	HTTPClient *http.Client

	// Checkers — опциональное переопределение CheckerFor по Kind, для тестов
	// (как у Runner). nil — используется пакетный CheckerFor.
	Checkers map[Kind]Checker

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

// httpClient возвращает клиент для походов в центр, собирая дефолтный ровно
// один раз: tick ходит в центр дважды в секунду, и новый http.Client на
// каждый поход — лишняя аллокация на горячем пути (соединения бы пулились и
// так, через общий http.DefaultTransport, но идиома пакета — собрать в init,
// как делает Runner).
func (c *ProbeClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.defaultClientOnce.Do(func() {
		c.defaultClient = &http.Client{Timeout: defaultProbeHTTPTimeout}
	})
	return c.defaultClient
}

// checkerFor resolves the Checker for kind — c.Checkers[kind] if the test
// injected one, otherwise the package-level CheckerFor.
func (c *ProbeClient) checkerFor(kind Kind) (Checker, error) {
	if ch, ok := c.Checkers[kind]; ok {
		return ch, nil
	}
	return CheckerFor(kind)
}

// Run — цикл пробы до отмены ctx: каждые PollEvery один тик (lease → проверки
// → results). Любая ошибка сети/5xx на lease или на results — лог и пропуск
// тика целиком: локального состояния проба не держит и результаты не копит,
// а центр вернёт незавершённые задания в очередь по истечении lease, и они
// приедут снова. Запускать горутиной.
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

// tick — один цикл: забрать задания, выполнить их пулом, отдать результаты.
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
			// Дальше пробовать нет смысла: если центр недоступен, недоступен и
			// для следующей пачки. Оставшиеся задания вернутся по lease.
			return
		}
	}
}

// runJobs выполняет задания пулом, ограниченным Concurrency, и собирает
// результаты. Возвращается, только когда отработали все запущенные проверки —
// иначе результат мог бы приехать после отправки пачки.
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
			// Shutdown посреди раздачи заданий: не запускаем новые проверки.
			// Нераспределённые задания останутся за пробой до истечения lease
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

// runOne выполняет одно задание. Ошибка самой проверки (сайт лежит, DNS не
// резолвится) — не ошибка, а нормальный Result{OK:false}, который поедет в
// центр как есть. Паника внутри чекера (баг в коде проверки) перехватывается
// и превращается в Result{OK:false, Error:"internal checker panic"} — как в
// Runner.runOne. ok=false означает «результата нет» (нет чекера под этот
// kind) — центру слать нечего, задание вернётся по истечении lease.
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
		return checker.Check(ctx, m)
	}()

	return NewResultDTO(j.QueueID, result), true
}

// lease забирает у центра порцию заданий своего региона.
func (c *ProbeClient) lease(ctx context.Context) ([]JobDTO, error) {
	var resp LeaseResponse
	if err := c.post(ctx, "/probe/lease", LeaseRequest{Limit: c.concurrency()}, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// postResults отдаёт центру пачку результатов (не больше maxResultsPerBatch —
// режет вызывающий).
func (c *ProbeClient) postResults(ctx context.Context, results []ResultDTO) error {
	var resp ResultsResponse
	if err := c.post(ctx, "/probe/results", ResultsRequest{Results: results}, &resp); err != nil {
		return err
	}
	if resp.Rejected > 0 {
		// Норма, а не сбой: lease истёк, пока проба чекала, или задание уже
		// выполнено другой пробой. Центр в таком случае вернёт его в очередь.
		slog.Warn("uptime: probe: results rejected by server", "accepted", resp.Accepted, "rejected", resp.Rejected)
	}
	return nil
}

// post — один POST в центр с Bearer-токеном и JSON-телом; ответ разбирается в
// out. Любой не-2xx — ошибка (тик пропускается вызывающим). Токен в тексте
// ошибки не появляется.
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
		// Тело ответа не логируем целиком — только код: центр отвечает
		// коротким JSON, но доверять его размеру незачем.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("uptime: probe: %s: unexpected status %d", path, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("uptime: probe: decode %s response: %w", path, err)
	}
	return nil
}
