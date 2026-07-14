package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// Pipeline — асинхронная обработка принятых событий:
// fingerprint → upsert issue (PG) → буфер батчера (CH).
type Pipeline struct {
	issues  *issue.Service
	batcher *event.Batcher
	queue   chan task
	workers int
	wg      sync.WaitGroup

	closeMu sync.RWMutex
	closed  bool
}

type task struct {
	projectID int64
	ev        *ParsedEvent
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
	ev := t.ev
	fp := fingerprint.Compute(fingerprint.Input{
		Custom:     ev.Fingerprint,
		Exceptions: ev.Exceptions,
		Message:    ev.Message,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := p.issues.Upsert(ctx,
		t.projectID, fp, ev.Title, ev.Culprit, ev.Level, ev.Timestamp)
	if err != nil {
		slog.Error("issue upsert failed, event dropped",
			"project_id", t.projectID, "event_id", ev.EventID, "error", err)
		return
	}
	// План 6: здесь сигналы new/regression уходят в alert.Evaluator.
	if res.New || res.Regression {
		slog.Debug("issue state change",
			"issue_id", res.IssueID, "new", res.New, "regression", res.Regression)
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
	})
}
