package ingest

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

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

func TestOTLPAttrDepthBounded(t *testing.T) {
	leaf := strings.Repeat("A", 4096)
	// Строим глубоко вложенный kvlist: {k={k={k=…{k=leaf}}}}
	v := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: leaf}}
	for i := 0; i < 2000; i++ {
		v = &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
			KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{Key: "k", Value: v}}},
		}}
	}

	done := make(chan string, 1)
	go func() { done <- otlpAttrString(v) }()
	select {
	case got := <-done:
		if len(got) > maxAttrLen*2 {
			t.Fatalf("вывод не ограничен: %d байт (кап %d)", len(got), maxAttrLen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("otlpAttrString не уложился в 5с — обход не ограничен по глубине")
	}
}
