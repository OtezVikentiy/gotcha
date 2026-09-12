package ingest_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func transactionJSONWithTrace(traceID string) string {
	end := time.Now().UTC()
	start := end.Add(-500 * time.Millisecond)
	unix := func(t time.Time) string {
		return fmt.Sprintf("%.6f", float64(t.UnixNano())/1e9)
	}
	return fmt.Sprintf(`{
	  "event_id":"5c2b2f5b1d1f4f2a9c3f6a7b8c9d0e1f",
	  "type":"transaction",
	  "transaction":"GET /api/users",
	  "start_timestamp":%s,
	  "timestamp":%s,
	  "environment":"prod",
	  "contexts":{"trace":{
	    "trace_id":%q,
	    "span_id":"bbbbbbbbbbbbbbbb",
	    "op":"http.server",
	    "status":"ok"
	  }},
	  "spans":[]
	}`, unix(start), unix(end), traceID)
}

func multiTransactionEnvelope(payloads []string) string {
	var b strings.Builder
	b.WriteString("{}\n")
	for _, p := range payloads {
		b.WriteString("{\"type\":\"transaction\"}\n")
		b.WriteString(strings.ReplaceAll(p, "\n", ""))
		b.WriteString("\n")
	}
	return b.String()
}

func sampledTraceIDs(n int, rate float64) (all []string, kept int) {
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%032x", i*0x9e3779b9)
		all = append(all, id)
		if trace.Keep(id, rate) {
			kept++
		}
	}
	return all, kept
}

func TestEnvelopeChargesOnlyStoredTransactions(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	const rate = 0.1
	if _, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = $2 WHERE id = $1", s.project.ID, rate); err != nil {
		t.Fatalf("set sample rate: %v", err)
	}

	ids, wantKept := sampledTraceIDs(40, rate)
	if wantKept == 0 || wantKept == len(ids) {
		t.Fatalf("набор трасс не разделился долей %v: сохранено %d из %d — тест ничего не проверяет",
			rate, wantKept, len(ids))
	}
	payloads := make([]string, 0, len(ids))
	for _, id := range ids {
		payloads = append(payloads, transactionJSONWithTrace(id))
	}

	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)
	resp := s.post(t, path, multiTransactionEnvelope(payloads), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	used, err := s.orgSvc.TransactionUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("TransactionUsage: %v", err)
	}
	if used != int64(wantKept) {
		t.Errorf("списано %d транзакций при %d сохраняемых из %d присланных: платим за то, что не хранится",
			used, wantKept, len(ids))
	}

	dropped, err := s.orgSvc.DroppedUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("DroppedUsage: %v", err)
	}
	if dropped.Transactions != 0 {
		t.Errorf("dropped_transactions = %d: отсеянное семплированием записано в потери по квоте",
			dropped.Transactions)
	}
}

func TestEnvelopeTracingDisabledChargesNothing(t *testing.T) {
	s := newStackWithoutTracing(t)
	ctx := context.Background()

	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)
	resp := s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	used, err := s.orgSvc.TransactionUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("TransactionUsage: %v", err)
	}
	if used != 0 {
		t.Errorf("при выключенном трейсинге списано %d транзакций — платим за то, чего не пишем", used)
	}
}

func TestOTLPChargesOnlyStoredTransactions(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = 0 WHERE id = $1", s.project.ID); err != nil {
		t.Fatalf("set sample rate: %v", err)
	}

	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))
	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	used, err := s.orgSvc.TransactionUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("TransactionUsage: %v", err)
	}
	if used != 0 {
		t.Errorf("списано %d транзакций при доле семплирования 0 — платим за то, что не хранится", used)
	}
	dropped, err := s.orgSvc.DroppedUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("DroppedUsage: %v", err)
	}
	if dropped.Transactions != 0 {
		t.Errorf("dropped_transactions = %d: отсеянное семплированием записано в потери по квоте",
			dropped.Transactions)
	}
}
