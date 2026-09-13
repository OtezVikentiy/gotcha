package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Возвращает списанную при приёме квоту за строки, которых не будет: писатель
// принял их за оплаченные, но потерял на буфере до записи в ClickHouse.
type DropRefunder interface {
	RefundMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error
	RefundProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error
}

// Резолв project_id → org_id отложен до фонового flush, а не сделан в самом
// SetDropSink-коллбэке — тот выполняется на горутине писателя, поход в PG там застопорил бы её флаш.
type WriterDropAttributor struct {
	Projects ProjectSettings
	Counter  DropCounter
	Refund   DropRefunder

	mu  sync.Mutex
	agg map[writerDropKey]int64

	// тестовый крюк: длинный интервал в тесте жизненного цикла отделяет флаш
	// по Close от флаша по тику.
	flushInterval time.Duration

	stop chan struct{}
	done chan struct{}
}

type writerDropKey struct {
	projectID uint64
	kind      dropKind
}

func NewWriterDropAttributor(projects ProjectSettings, counter DropCounter, refund DropRefunder) *WriterDropAttributor {
	return &WriterDropAttributor{
		Projects:      projects,
		Counter:       counter,
		Refund:        refund,
		agg:           make(map[writerDropKey]int64),
		flushInterval: writerDropFlushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

func (a *WriterDropAttributor) CountDroppedMetrics(projectID uint64, n int64) {
	a.add(projectID, dropMetric, n)
}

func (a *WriterDropAttributor) CountDroppedProfiles(projectID uint64, n int64) {
	a.add(projectID, dropProfile, n)
}

func (a *WriterDropAttributor) add(projectID uint64, kind dropKind, n int64) {
	if projectID == 0 || n <= 0 {
		return
	}
	a.mu.Lock()
	a.agg[writerDropKey{projectID: projectID, kind: kind}] += n
	a.mu.Unlock()
}

const writerDropFlushInterval = 20 * time.Second

const writerDropFlushTimeout = 5 * time.Second

func (a *WriterDropAttributor) Run() {
	defer close(a.done)
	ticker := time.NewTicker(a.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			a.flush(context.Background())
			return
		case <-ticker.C:
			a.flush(context.Background())
		}
	}
}

func (a *WriterDropAttributor) Close() {
	close(a.stop)
	<-a.done
}

func (a *WriterDropAttributor) drain() map[writerDropKey]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.agg) == 0 {
		return nil
	}
	out := a.agg
	a.agg = make(map[writerDropKey]int64, len(out))
	return out
}

// best-effort, как Pipeline.flushDropped: резолв или запись не удались — окно
// потери логируется и не ретраится, drain уже забрал накопленное.
func (a *WriterDropAttributor) flush(parent context.Context) {
	agg := a.drain()
	if len(agg) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, writerDropFlushTimeout)
	defer cancel()
	month := time.Now().UTC()
	for key, n := range agg {
		proj, err := a.Projects.Resolve(ctx, int64(key.projectID))
		if err != nil {
			slog.Warn("ingest: writer drop attribution failed to resolve project, count lost",
				"project_id", key.projectID, "kind", key.kind, "n", n, "error", err)
			continue
		}
		a.report(ctx, key.kind, proj.OrgID, month, n)
	}
}

func (a *WriterDropAttributor) report(ctx context.Context, kind dropKind, orgID int64, month time.Time, n int64) {
	var incErr, refundErr error
	switch kind {
	case dropMetric:
		incErr = a.Counter.IncDroppedMetrics(ctx, orgID, month, n)
		refundErr = a.Refund.RefundMetrics(ctx, orgID, month, n)
	case dropProfile:
		incErr = a.Counter.IncDroppedProfiles(ctx, orgID, month, n)
		refundErr = a.Refund.RefundProfiles(ctx, orgID, month, n)
	}
	if incErr != nil {
		slog.Warn("ingest: writer drop counter update failed",
			"org_id", orgID, "kind", kind, "n", n, "error", incErr)
	}
	if refundErr != nil {
		slog.Warn("ingest: writer drop quota refund failed",
			"org_id", orgID, "kind", kind, "n", n, "error", refundErr)
	}
}
