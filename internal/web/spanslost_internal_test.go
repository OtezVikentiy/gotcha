package web

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// Обе оборонительные ветки должны склоняться к «потеряно», а не к «истёк
// срок»: соврать в пользу retention — хуже, чем перестраховаться в лог.
func TestSpansLookLostDefaultsToLossOnUncertainty(t *testing.T) {
	ch := testenv.MigratedCH(t)
	h := &Handler{Trace: trace.NewQuery(ch), SpanRetentionDays: 30}

	w := trace.NewSpanWriter(ch)
	go w.Run()

	const projectID = int64(777)
	const traceID = "spanslost-recent"
	start := time.Now().UTC().Add(-time.Hour)
	w.Add(projectID, projectID, trace.Transaction{
		TraceID: traceID, SpanID: "root", Name: "GET /x", Op: "http.server",
		Status: "ok", Start: start, End: start.Add(10 * time.Millisecond), Environment: "production",
	})
	flushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.Close(flushCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := h.spansLookLost(context.Background(), projectID, "spanslost-unknown"); !got {
		t.Errorf("unknown trace: spansLookLost = false, want true (!found обязан склоняться к потере)")
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got := h.spansLookLost(canceled, projectID, traceID); !got {
		t.Errorf("отменённый ctx: spansLookLost = false, want true (ошибка резолва обязана склоняться к потере)")
	}
}
