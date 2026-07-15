package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// perfDetectBudget — бюджет ВСЕЙ детекции по одной транзакции: чтение настроек
// проекта, запись всех находок и их алерты. Бюджет именно общий, а не на каждую
// находку: с бюджетом на находку транзакция с максимумом находок
// (maxFindingsPerTransaction = 20) при медленной PG удерживала бы одного из
// четырёх воркеров до 20 x 5с ≈ 100с — а через ту же очередь на 1000 слотов идут
// события об ОШИБКАХ, и они в это время дропаются с warn-логом. Приём ошибок
// важнее полноты детекции: хвост находок, не поместившийся в бюджет,
// пропускается с warn-логом (та же проблема найдётся на следующей транзакции —
// она воспроизводится на каждом запросе к эндпойнту).
const perfDetectBudget = 10 * time.Second

// AlertSink получает сигналы о смене состояния issue (новая группа,
// регрессия), чтобы решить, нужно ли поставить уведомления в очередь.
// Отдельный интерфейс (а не прямая зависимость от *alert.Evaluator) держит
// Pipeline тестируемым без реальной БД под алертингом и делает поле
// необязательным: nil (см. Pipeline.Alerts) значит "алертинг выключен".
type AlertSink interface {
	OnIssue(ctx context.Context, ev alert.Event)
}

// SpanSink принимает семплированные транзакции для записи в ClickHouse;
// *trace.SpanWriter ему удовлетворяет. Отдельный интерфейс (а не прямая
// зависимость от *trace.SpanWriter) держит Pipeline тестируемым без CH и
// делает поле необязательным: nil (см. Pipeline.Spans) значит «трейсинг
// выключен».
type SpanSink interface {
	Add(projectID int64, t trace.Transaction)
}

// PerfSink записывает находку детекторов производительности в perf_issues (PG);
// *trace.IssueService ему удовлетворяет. Отдельный интерфейс (а не прямая
// зависимость от *trace.IssueService) держит Pipeline тестируемым без PG и
// делает поле необязательным: nil (см. Pipeline.Perf) значит «детекторы
// выключены».
type PerfSink interface {
	Record(ctx context.Context, projectID int64, f trace.Finding, traceID string) (trace.RecordResult, error)
}

// PerfNotifier алертит о ПЕРВОМ обнаружении проблемы производительности и о её
// регрессии (была resolved — снова обнаружена); *trace.OutboxNotifier ему
// удовлетворяет. nil (см. Pipeline.PerfAlerts) — алерты по производительности
// выключены, детекция всё равно идёт.
type PerfNotifier interface {
	NotifyNew(ctx context.Context, projectID int64, iss trace.PerfIssue) error
	NotifyRegression(ctx context.Context, projectID int64, iss trace.PerfIssue) error
}

// Pipeline — асинхронная обработка принятых событий:
// fingerprint → upsert issue (PG) → буфер батчера (CH). Транзакции идут через
// ту же очередь вторым типом задачи: у них нет ни fingerprint'а, ни issue —
// запись в SpanSink (CH) и детекция проблем производительности (PG + outbox).
type Pipeline struct {
	issues  *issue.Service
	batcher *event.Batcher
	queue   chan task
	workers int
	wg      sync.WaitGroup

	// Alerts — опциональный колбэк для new_issue/regression (план 6).
	// nil (значение по умолчанию) означает, что алертинг выключен —
	// process() просто пропускает вызов.
	Alerts AlertSink

	// Spans — приёмник транзакций; nil означает, что трейсинг выключен и
	// Handler не принимает transaction-item'ы (см. TracingEnabled).
	Spans SpanSink

	// Perf — запись находок детекторов в perf_issues; nil выключает детекцию.
	Perf PerfSink

	// PerfAlerts — алерт при первом обнаружении проблемы; nil выключает алерты
	// (детекция при этом продолжает работать).
	PerfAlerts PerfNotifier

	// Projects — источник настроек проекта, из которого детекция берёт пороги
	// (projects.perf_detector_config); nil означает «на дефолтах».
	Projects ProjectSettings

	// testPerfBudget подменяет perfDetectBudget в тестах; 0 — обычный бюджет.
	testPerfBudget time.Duration

	closeMu sync.RWMutex
	closed  bool
}

// task — единица работы воркера: ЛИБО событие (ev), ЛИБО транзакция (tx).
type task struct {
	projectID int64
	ev        *ParsedEvent
	tx        *trace.Transaction
}

func NewPipeline(issues *issue.Service, batcher *event.Batcher) *Pipeline {
	return &Pipeline{
		issues:  issues,
		batcher: batcher,
		queue:   make(chan task, 1000),
		workers: 4,
	}
}

func (p *Pipeline) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for t := range p.queue {
				p.process(t)
			}
		}()
	}
}

// Enqueue не блокирует: при полной очереди событие дропается с warn-логом —
// приём ошибок не должен вставать из-за медленной обработки. После Close
// событие тоже дропается — send в закрытый канал иначе паникует, если
// in-flight HTTP-хендлер зовёт Enqueue параллельно с drain'ом.
func (p *Pipeline) Enqueue(projectID int64, ev *ParsedEvent) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		slog.Warn("ingest pipeline closed, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
		return
	}
	select {
	case p.queue <- task{projectID: projectID, ev: ev}:
	default:
		slog.Warn("ingest queue full, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
	}
}

// TracingEnabled сообщает, есть ли куда писать транзакции. Handler смотрит на
// это до квоты: не тратить бюджет транзакций организации, если писать их
// всё равно некуда.
func (p *Pipeline) TracingEnabled() bool {
	return p.Spans != nil
}

// EnqueueTransaction — как Enqueue, но для транзакции: не блокирует, дропает
// с warn-логом при полной очереди или после Close.
func (p *Pipeline) EnqueueTransaction(projectID int64, tx trace.Transaction) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		slog.Warn("ingest pipeline closed, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return
	}
	select {
	case p.queue <- task{projectID: projectID, tx: &tx}:
	default:
		slog.Warn("ingest queue full, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
	}
}

// Close перестаёт принимать и дожидается обработки очереди. Идемпотентен.
func (p *Pipeline) Close() {
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		return
	}
	p.closed = true
	close(p.queue)
	p.closeMu.Unlock()
	p.wg.Wait()
}

func (p *Pipeline) process(t task) {
	if t.tx != nil {
		p.processTransaction(t.projectID, *t.tx)
		return
	}
	ev := t.ev
	fp := fingerprint.Compute(fingerprint.Input{
		Custom:     ev.Fingerprint,
		Exceptions: ev.Exceptions,
		Message:    ev.Message,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := p.issues.Upsert(ctx,
		t.projectID, fp, ev.Title, ev.Culprit, ev.Level, ev.Environment, ev.Timestamp)
	if err != nil {
		slog.Error("issue upsert failed, event dropped",
			"project_id", t.projectID, "event_id", ev.EventID, "error", err)
		return
	}
	if (res.New || res.Regression) && p.Alerts != nil {
		kind := alert.KindNewIssue
		if res.Regression {
			kind = alert.KindRegression
		}
		// times_seen требует отдельного чтения: Upsert его не возвращает.
		// New/Regression — редкие переходы состояния (не каждое событие),
		// так что лишний round-trip к PG здесь не на горячем пути приёма.
		timesSeen := int64(1)
		if iss, err := p.issues.Get(ctx, res.IssueID); err != nil {
			slog.Error("issue lookup for alert failed", "issue_id", res.IssueID, "error", err)
		} else {
			timesSeen = iss.TimesSeen
		}
		p.Alerts.OnIssue(ctx, alert.Event{
			ProjectID: t.projectID,
			IssueID:   res.IssueID,
			Kind:      kind,
			Title:     ev.Title,
			Culprit:   ev.Culprit,
			Level:     ev.Level,
			TimesSeen: timesSeen,
		})
	}

	var excType, excValue string
	if n := len(ev.Exceptions); n > 0 {
		excType, excValue = ev.Exceptions[n-1].Type, ev.Exceptions[n-1].Value
	}
	p.batcher.Add(event.Event{
		ID:             ev.EventID,
		ProjectID:      t.projectID,
		IssueID:        res.IssueID,
		Timestamp:      ev.Timestamp,
		Level:          ev.Level,
		Message:        ev.Message,
		ExceptionType:  excType,
		ExceptionValue: excValue,
		Stacktrace:     ev.StacktraceJSON,
		Environment:    ev.Environment,
		Release:        ev.Release,
		ServerName:     ev.ServerName,
		SDK:            ev.SDK,
		UserID:         ev.UserID,
		UserIP:         ev.UserIP,
		UserEmail:      ev.UserEmail,
		Tags:           ev.Tags,
		Contexts:       ev.ContextsJSON,
		TraceID:        ev.TraceID,
		SpanID:         ev.SpanID,
	})
}

// processTransaction пишет транзакцию в SpanWriter и прогоняет по ней детекторы
// производительности. Порядок важен: Spans.Add идёт ПЕРВЫМ, и запись в CH не
// ждёт ни PG, ни outbox — трейс попадает в хранилище независимо от того, что
// случится в детекции.
func (p *Pipeline) processTransaction(projectID int64, tx trace.Transaction) {
	if p.Spans == nil { // трейсинг выключен — Handler сюда не должен доходить
		slog.Warn("tracing disabled, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return
	}
	p.Spans.Add(projectID, tx)
	p.detectPerfIssues(projectID, tx)
}

// detectPerfIssues прогоняет детекторы по спанам транзакции, апсертит находки в
// perf_issues и алертит о тех, что увидены впервые или вернулись после resolve.
//
// Детекция не имеет права ронять приём: паника детектора или сбой PG здесь
// логируются и на этом заканчиваются — транзакция уже записана в CH (см.
// processTransaction), а воркер продолжает разбирать очередь.
func (p *Pipeline) detectPerfIssues(projectID int64, tx trace.Transaction) {
	if p.Perf == nil { // детекторы выключены
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("perf detection panicked, transaction still written",
				"project_id", projectID, "trace_id", tx.TraceID, "panic", r)
		}
	}()

	// ОДИН бюджет на всю детекцию: настройки, все Record и все алерты (см.
	// perfDetectBudget). Дольше него воркер этой транзакцией не занимается.
	ctx, cancel := context.WithTimeout(context.Background(), p.perfBudget())
	defer cancel()

	cfg := p.detectorConfig(ctx, projectID)

	findings := trace.Detect(tx, cfg)
	for i, f := range findings {
		if err := ctx.Err(); err != nil {
			slog.Warn("perf detection budget exhausted, remaining findings skipped",
				"project_id", projectID, "trace_id", tx.TraceID,
				"recorded", i, "skipped", len(findings)-i, "error", err)
			return
		}
		p.recordFinding(ctx, projectID, tx, f)
	}
}

// perfBudget — бюджет детекции; отдельный метод, чтобы тесты подменяли его через
// поле, не трогая глобальную переменную.
func (p *Pipeline) perfBudget() time.Duration {
	if p.testPerfBudget > 0 {
		return p.testPerfBudget
	}
	return perfDetectBudget
}

// recordFinding пишет одну находку и алертит о ней, если она новая или вернулась.
// ctx — общий бюджет детекции (см. detectPerfIssues), а не персональный.
func (p *Pipeline) recordFinding(ctx context.Context, projectID int64, tx trace.Transaction, f trace.Finding) {
	res, err := p.Perf.Record(ctx, projectID, f, tx.TraceID)
	if err != nil {
		slog.Error("perf issue record failed",
			"project_id", projectID, "trace_id", tx.TraceID, "kind", f.Kind, "error", err)
		return
	}
	// Алерт — при ПЕРВОМ обнаружении и при регрессии (проблему починили, и она
	// вернулась). На повторные обнаружения — молчим: проблема воспроизводится на
	// каждом запросе к эндпойнту, и алерт на каждое повторение был бы лавиной.
	if p.PerfAlerts == nil || (!res.Created && !res.Regression) {
		return
	}
	notify := p.PerfAlerts.NotifyNew
	if res.Regression {
		notify = p.PerfAlerts.NotifyRegression
	}
	if err := notify(ctx, projectID, res.Issue); err != nil {
		slog.Error("perf issue alert failed", "project_id", projectID,
			"perf_issue_id", res.Issue.ID, "regression", res.Regression, "error", err)
	}
}

// detectorConfig — пороги проекта (projects.perf_detector_config). Любая
// проблема с их чтением или разбором — не повод не детектить: возвращаются
// дефолты.
func (p *Pipeline) detectorConfig(ctx context.Context, projectID int64) trace.DetectorConfig {
	if p.Projects == nil {
		return trace.DefaultDetectorConfig()
	}
	proj, err := p.Projects.Resolve(ctx, projectID)
	if err != nil {
		slog.Error("perf detector config lookup failed, using defaults",
			"project_id", projectID, "error", err)
		return trace.DefaultDetectorConfig()
	}
	cfg, err := trace.ConfigFromJSON([]byte(proj.PerfDetectorConfig))
	if err != nil {
		slog.Error("perf detector config parse failed, using defaults",
			"project_id", projectID, "error", err)
	}
	return cfg
}
