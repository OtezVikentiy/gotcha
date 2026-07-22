package ingest

import (
	"encoding/binary"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestMapOTLPCapsSpans — потолок числа спанов на один OTLP /v1/traces запрос:
// свыше maxOTLPSpans разбор прекращается (защита от амплификации памяти/CPU
// недоверенным экспортом с сотнями тысяч спанов).
func TestMapOTLPCapsSpans(t *testing.T) {
	now := time.Now().UTC()
	start := uint64(now.Add(-time.Minute).UnixNano())
	end := uint64(now.Add(-time.Minute + 10*time.Millisecond).UnixNano())

	spans := make([]*tracepb.Span, 0, maxOTLPSpans+100)
	for i := 0; i < maxOTLPSpans+100; i++ {
		tid := make([]byte, 16)
		sid := make([]byte, 8)
		binary.BigEndian.PutUint64(tid[8:], uint64(i)+1) // уникальный ненулевой trace_id
		binary.BigEndian.PutUint64(sid, uint64(i)+1)     // уникальный ненулевой span_id
		spans = append(spans, &tracepb.Span{
			TraceId:           tid,
			SpanId:            sid,
			Name:              "op",
			StartTimeUnixNano: start,
			EndTimeUnixNano:   end,
			// без parent_span_id → корень → своя транзакция
		})
	}
	rs := []*tracepb.ResourceSpans{resSpans(nil, spans...)}

	out := MapOTLP(rs, now)
	if len(out) != maxOTLPSpans {
		t.Fatalf("MapOTLP capped transactions = %d, want %d (flat span cap)", len(out), maxOTLPSpans)
	}
}
