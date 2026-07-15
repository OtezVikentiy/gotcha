package ingest

import (
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// --- хелперы сборки protobuf-структур (без сети и без docker) ---

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v},
	}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: v},
	}}
}

func boolAttr(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_BoolValue{BoolValue: v},
	}}
}

func dblAttr(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v},
	}}
}

func nanos(ts time.Time) uint64 { return uint64(ts.UnixNano()) }

// resSpans собирает один ResourceSpans с одним ScopeSpans.
func resSpans(resAttrs []*commonpb.KeyValue, spans ...*tracepb.Span) *tracepb.ResourceSpans {
	return &tracepb.ResourceSpans{
		Resource:   &resourcepb.Resource{Attributes: resAttrs},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}
}

var (
	traceIDBytes = []byte{0xAB, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0xFF}
	rootIDBytes  = []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	dbIDBytes    = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	httpIDBytes  = []byte{0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xF0, 0x01}
)

func TestMapOTLP(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(500 * time.Millisecond)

	rootSpan := func(mods ...func(*tracepb.Span)) *tracepb.Span {
		s := &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            rootIDBytes,
			Name:              "GET /checkout",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
			Attributes: []*commonpb.KeyValue{
				strAttr("http.request.method", "GET"),
				strAttr("url.path", "/checkout"),
				intAttr("http.response.status_code", 200),
			},
			Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		}
		for _, m := range mods {
			m(s)
		}
		return s
	}

	dbSpan := &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            dbIDBytes,
		ParentSpanId:      rootIDBytes,
		Name:              "SELECT orders",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: nanos(start.Add(10 * time.Millisecond)),
		EndTimeUnixNano:   nanos(start.Add(30 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			strAttr("db.system", "postgresql"),
			strAttr("db.statement", "SELECT * FROM orders WHERE id = $1"),
			intAttr("db.rows_affected", 7),
		},
	}

	httpSpan := &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            httpIDBytes,
		ParentSpanId:      rootIDBytes,
		Name:              "HTTP GET",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: nanos(start.Add(40 * time.Millisecond)),
		EndTimeUnixNano:   nanos(start.Add(90 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			strAttr("http.request.method", "GET"),
			strAttr("url.full", "https://api.example.com/v1/pay"),
		},
		Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
	}

	resAttrs := []*commonpb.KeyValue{
		strAttr("service.name", "checkout"),
		strAttr("deployment.environment", "production"),
		strAttr("service.version", "1.2.3"),
	}

	tests := []struct {
		name  string
		rs    []*tracepb.ResourceSpans
		check func(*testing.T, []trace.Transaction)
	}{
		{
			name: "корень SERVER + db- и http-дети",
			rs:   []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), dbSpan, httpSpan)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 {
					t.Fatalf("транзакций: %d, ждали 1", len(txs))
				}
				tx := txs[0]
				if tx.TraceID != hex.EncodeToString(traceIDBytes) {
					t.Errorf("TraceID = %q", tx.TraceID)
				}
				if tx.TraceID != "ab0102030405060708090a0b0c0d0eff" {
					t.Errorf("TraceID не lowercase hex: %q", tx.TraceID)
				}
				if tx.SpanID != "deadbeef01020304" {
					t.Errorf("SpanID = %q, ждали lowercase hex", tx.SpanID)
				}
				if tx.Name != "GET /checkout" {
					t.Errorf("Name = %q", tx.Name)
				}
				if tx.Op != "http.server" {
					t.Errorf("Op = %q, ждали http.server", tx.Op)
				}
				if tx.Status != "ok" {
					t.Errorf("Status = %q, ждали ok", tx.Status)
				}
				if tx.Source != "otlp" {
					t.Errorf("Source = %q", tx.Source)
				}
				if !tx.Start.Equal(start) || !tx.End.Equal(end) {
					t.Errorf("время: %v..%v, ждали %v..%v", tx.Start, tx.End, start, end)
				}
				if tx.ServerName != "checkout" || tx.Environment != "production" || tx.Release != "1.2.3" {
					t.Errorf("resource: server=%q env=%q release=%q", tx.ServerName, tx.Environment, tx.Release)
				}
				if len(tx.Spans) != 2 {
					t.Fatalf("спанов: %d, ждали 2", len(tx.Spans))
				}
				db := tx.Spans[0]
				if db.SpanID != "1122334455667788" || db.ParentSpanID != "deadbeef01020304" {
					t.Errorf("db ids: %q / %q", db.SpanID, db.ParentSpanID)
				}
				if db.Op != "db" {
					t.Errorf("db.Op = %q", db.Op)
				}
				if db.Description != "SELECT * FROM orders WHERE id = $1" {
					t.Errorf("db.Description = %q", db.Description)
				}
				if db.Status != "ok" {
					t.Errorf("db.Status = %q", db.Status)
				}
				if got := db.Data["db.rows_affected"]; got != "7" {
					t.Errorf("db.Data[db.rows_affected] = %#v, ждали строку \"7\"", got)
				}
				h := tx.Spans[1]
				if h.Op != "http.client" {
					t.Errorf("http.Op = %q", h.Op)
				}
				if h.Description != "GET https://api.example.com/v1/pay" {
					t.Errorf("http.Description = %q", h.Description)
				}
				if h.Status != "internal_error" {
					t.Errorf("http.Status = %q, ждали internal_error", h.Status)
				}
			},
		},
		{
			name: "deployment.environment.name как альтернатива",
			rs: []*tracepb.ResourceSpans{resSpans([]*commonpb.KeyValue{
				strAttr("service.name", "worker"),
				strAttr("deployment.environment.name", "staging"),
			}, rootSpan())},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || txs[0].Environment != "staging" {
					t.Fatalf("txs = %+v", txs)
				}
			},
		},
		{
			name: "спан-сирота: корень не приехал в этом запросе",
			rs:   []*tracepb.ResourceSpans{resSpans(resAttrs, dbSpan)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (сирота отброшен)", len(txs))
				}
			},
		},
		{
			name: "корень со статусом ERROR → internal_error",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || txs[0].Status != "internal_error" {
					t.Fatalf("txs = %+v", txs)
				}
			},
		},
		{
			name: "статус UNSET → ok (как в Sentry-парсере)",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.Status = nil
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || txs[0].Status != "ok" {
					t.Fatalf("txs = %+v", txs)
				}
			},
		},
		{
			name: "таймстемп старше окна хранения → транзакции нет",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				old := now.Add(-100 * 24 * time.Hour)
				s.StartTimeUnixNano = nanos(old)
				s.EndTimeUnixNano = nanos(old.Add(time.Second))
			}), dbSpan)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (вне окна)", len(txs))
				}
			},
		},
		{
			name: "таймстемп далеко в будущем → транзакции нет",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				future := now.Add(48 * time.Hour)
				s.StartTimeUnixNano = nanos(future)
				s.EndTimeUnixNano = nanos(future.Add(time.Second))
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (будущее)", len(txs))
				}
			},
		},
		{
			name: "спан вне окна выкидывается, транзакция остаётся",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), &tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            dbIDBytes,
				ParentSpanId:      rootIDBytes,
				Name:              "poison",
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: nanos(now.Add(-200 * 24 * time.Hour)),
				EndTimeUnixNano:   nanos(now.Add(-200 * 24 * time.Hour).Add(time.Second)),
			})},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 {
					t.Fatalf("транзакций: %d, ждали 1", len(txs))
				}
				if len(txs[0].Spans) != 0 {
					t.Fatalf("спанов: %d, ждали 0 (спан вне окна выброшен)", len(txs[0].Spans))
				}
			},
		},
		{
			name: "нулевой trace_id → спан отброшен",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.TraceId = make([]byte, 16) // все нули — невалидный id
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (нулевой trace_id)", len(txs))
				}
			},
		},
		{
			name: "пустой trace_id → спан отброшен",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.TraceId = nil
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (пустой trace_id)", len(txs))
				}
			},
		},
		{
			name: "числовые и булевы атрибуты корня → Tags строками",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.Attributes = []*commonpb.KeyValue{
					strAttr("http.route", "/checkout"),
					intAttr("http.response.status_code", 500),
					boolAttr("feature.new_flow", true),
					dblAttr("sample.ratio", 0.25),
				}
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 {
					t.Fatalf("транзакций: %d", len(txs))
				}
				want := map[string]string{
					"http.route":                "/checkout",
					"http.response.status_code": "500",
					"feature.new_flow":          "true",
					"sample.ratio":              "0.25",
				}
				for k, v := range want {
					if got := txs[0].Tags[k]; got != v {
						t.Errorf("Tags[%q] = %q, ждали %q", k, got, v)
					}
				}
			},
		},
		{
			name: "kind без http/db → op = span.kind в нижнем регистре",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs,
				rootSpan(func(s *tracepb.Span) {
					s.Kind = tracepb.Span_SPAN_KIND_CONSUMER
					s.Attributes = nil
					s.ParentSpanId = []byte{0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08} // родителя нет в запросе, но CONSUMER — корень
				}),
				&tracepb.Span{
					TraceId:           traceIDBytes,
					SpanId:            dbIDBytes,
					ParentSpanId:      rootIDBytes,
					Name:              "compute",
					Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
					StartTimeUnixNano: nanos(start),
					EndTimeUnixNano:   nanos(end),
				},
				&tracepb.Span{
					TraceId:           traceIDBytes,
					SpanId:            httpIDBytes,
					ParentSpanId:      rootIDBytes,
					Name:              "publish",
					Kind:              tracepb.Span_SPAN_KIND_PRODUCER,
					StartTimeUnixNano: nanos(start),
					EndTimeUnixNano:   nanos(end),
				},
			)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 {
					t.Fatalf("транзакций: %d, ждали 1 (CONSUMER — корень)", len(txs))
				}
				if txs[0].Op != "consumer" {
					t.Errorf("корень Op = %q, ждали consumer", txs[0].Op)
				}
				if len(txs[0].Spans) != 2 {
					t.Fatalf("спанов: %d", len(txs[0].Spans))
				}
				if txs[0].Spans[0].Op != "internal" || txs[0].Spans[1].Op != "producer" {
					t.Errorf("ops: %q, %q", txs[0].Spans[0].Op, txs[0].Spans[1].Op)
				}
				if txs[0].Spans[0].Description != "compute" {
					t.Errorf("Description = %q, ждали имя спана", txs[0].Spans[0].Description)
				}
			},
		},
		{
			name: "CLIENT без http-метода и без db → op = client",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), &tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            dbIDBytes,
				ParentSpanId:      rootIDBytes,
				Name:              "grpc call",
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: nanos(start),
				EndTimeUnixNano:   nanos(end),
			})},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || len(txs[0].Spans) != 1 {
					t.Fatalf("txs = %+v", txs)
				}
				if txs[0].Spans[0].Op != "client" {
					t.Errorf("Op = %q, ждали client", txs[0].Spans[0].Op)
				}
			},
		},
		{
			name: "устаревшие атрибуты: http.method + http.url, db.query.text",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(),
				&tracepb.Span{
					TraceId:           traceIDBytes,
					SpanId:            httpIDBytes,
					ParentSpanId:      rootIDBytes,
					Name:              "HTTP POST",
					Kind:              tracepb.Span_SPAN_KIND_CLIENT,
					StartTimeUnixNano: nanos(start),
					EndTimeUnixNano:   nanos(end),
					Attributes: []*commonpb.KeyValue{
						strAttr("http.method", "POST"),
						strAttr("http.url", "https://legacy.example.com/pay"),
					},
				},
				&tracepb.Span{
					TraceId:           traceIDBytes,
					SpanId:            dbIDBytes,
					ParentSpanId:      rootIDBytes,
					Name:              "query",
					Kind:              tracepb.Span_SPAN_KIND_CLIENT,
					StartTimeUnixNano: nanos(start),
					EndTimeUnixNano:   nanos(end),
					Attributes: []*commonpb.KeyValue{
						strAttr("db.system", "mysql"),
						strAttr("db.query.text", "SELECT 1"),
					},
				},
			)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || len(txs[0].Spans) != 2 {
					t.Fatalf("txs = %+v", txs)
				}
				h, db := txs[0].Spans[0], txs[0].Spans[1]
				if h.Op != "http.client" || h.Description != "POST https://legacy.example.com/pay" {
					t.Errorf("http: op=%q desc=%q", h.Op, h.Description)
				}
				if db.Op != "db" || db.Description != "SELECT 1" {
					t.Errorf("db: op=%q desc=%q", db.Op, db.Description)
				}
			},
		},
		{
			name: "http.client без url.full: server.address + url.path",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), &tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            httpIDBytes,
				ParentSpanId:      rootIDBytes,
				Name:              "GET",
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: nanos(start),
				EndTimeUnixNano:   nanos(end),
				Attributes: []*commonpb.KeyValue{
					strAttr("http.request.method", "GET"),
					strAttr("server.address", "api.internal"),
					strAttr("url.path", "/v2/users"),
				},
			})},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || len(txs[0].Spans) != 1 {
					t.Fatalf("txs = %+v", txs)
				}
				if got := txs[0].Spans[0].Description; got != "GET api.internal/v2/users" {
					t.Errorf("Description = %q", got)
				}
			},
		},
		{
			name: "события спана попадают в Data",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), &tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            dbIDBytes,
				ParentSpanId:      rootIDBytes,
				Name:              "work",
				Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
				StartTimeUnixNano: nanos(start),
				EndTimeUnixNano:   nanos(end),
				Events: []*tracepb.Span_Event{{
					TimeUnixNano: nanos(start),
					Name:         "exception",
					Attributes:   []*commonpb.KeyValue{strAttr("exception.type", "TimeoutError")},
				}},
			})},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || len(txs[0].Spans) != 1 {
					t.Fatalf("txs = %+v", txs)
				}
				events, ok := txs[0].Spans[0].Data["events"].([]any)
				if !ok || len(events) != 1 {
					t.Fatalf("Data[events] = %#v", txs[0].Spans[0].Data["events"])
				}
				ev, ok := events[0].(map[string]any)
				if !ok || ev["name"] != "exception" {
					t.Fatalf("event = %#v", events[0])
				}
				attrs, ok := ev["attributes"].(map[string]any)
				if !ok || attrs["exception.type"] != "TimeoutError" {
					t.Errorf("event attributes = %#v", ev["attributes"])
				}
			},
		},
		{
			name: "два трейса в одном запросе → две транзакции, дети не перемешиваются",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), dbSpan), resSpans(
				[]*commonpb.KeyValue{strAttr("service.name", "billing")},
				&tracepb.Span{
					TraceId:           []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09},
					SpanId:            rootIDBytes,
					Name:              "POST /charge",
					Kind:              tracepb.Span_SPAN_KIND_SERVER,
					StartTimeUnixNano: nanos(start),
					EndTimeUnixNano:   nanos(end),
				},
			)},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 2 {
					t.Fatalf("транзакций: %d, ждали 2", len(txs))
				}
				if len(txs[0].Spans) != 1 || len(txs[1].Spans) != 0 {
					t.Errorf("спаны: %d и %d", len(txs[0].Spans), len(txs[1].Spans))
				}
				if txs[0].ServerName != "checkout" || txs[1].ServerName != "billing" {
					t.Errorf("service: %q, %q", txs[0].ServerName, txs[1].ServerName)
				}
			},
		},
		{
			name: "http.client: url.path без ведущего слэша",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(), &tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            httpIDBytes,
				ParentSpanId:      rootIDBytes,
				Name:              "GET",
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: nanos(start),
				EndTimeUnixNano:   nanos(end),
				Attributes: []*commonpb.KeyValue{
					strAttr("http.request.method", "GET"),
					strAttr("server.address", "api.internal"),
					strAttr("url.path", "v2/users"), // без слэша: разделитель наш
				},
			})},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 1 || len(txs[0].Spans) != 1 {
					t.Fatalf("txs = %+v", txs)
				}
				if got := txs[0].Spans[0].Description; got != "GET api.internal/v2/users" {
					t.Errorf("Description = %q, ждали GET api.internal/v2/users", got)
				}
			},
		},
		{
			name: "корень с нулевым span_id → спан отброшен",
			rs: []*tracepb.ResourceSpans{resSpans(resAttrs, rootSpan(func(s *tracepb.Span) {
				s.SpanId = make([]byte, 8) // все нули — невалиден по спеке, как и trace_id
			}))},
			check: func(t *testing.T, txs []trace.Transaction) {
				if len(txs) != 0 {
					t.Fatalf("транзакций: %d, ждали 0 (нулевой span_id)", len(txs))
				}
			},
		},
		{
			name:  "пустой вход",
			rs:    nil,
			check: func(t *testing.T, txs []trace.Transaction) { checkEmpty(t, txs) },
		},
		{
			name:  "nil ResourceSpans не паникует",
			rs:    []*tracepb.ResourceSpans{nil, {}},
			check: func(t *testing.T, txs []trace.Transaction) { checkEmpty(t, txs) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, MapOTLP(tt.rs, now))
		})
	}
}

func checkEmpty(t *testing.T, txs []trace.Transaction) {
	t.Helper()
	if len(txs) != 0 {
		t.Fatalf("транзакций: %d, ждали 0", len(txs))
	}
}

// TestMapOTLPLimits — недоверенные строки каппятся теми же лимитами, что и в
// Sentry-парсере (имя 200, description 2000, op 100).
func TestMapOTLPLimits(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	long := func(n int) string {
		b := make([]rune, n)
		for i := range b {
			b[i] = 'x'
		}
		return string(b)
	}

	rs := []*tracepb.ResourceSpans{resSpans(
		[]*commonpb.KeyValue{strAttr("service.name", long(500))},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            rootIDBytes,
			Name:              long(500),
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(start.Add(time.Second)),
		},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            dbIDBytes,
			ParentSpanId:      rootIDBytes,
			Name:              "q",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(start.Add(time.Second)),
			Attributes: []*commonpb.KeyValue{
				strAttr("db.system", "postgresql"),
				strAttr("db.statement", long(5000)),
			},
		},
	)}

	txs := MapOTLP(rs, now)
	if len(txs) != 1 || len(txs[0].Spans) != 1 {
		t.Fatalf("txs = %+v", txs)
	}
	if got := len([]rune(txs[0].Name)); got != maxTransactionName {
		t.Errorf("len(Name) = %d, ждали %d", got, maxTransactionName)
	}
	if got := len([]rune(txs[0].ServerName)); got != 200 {
		t.Errorf("len(ServerName) = %d, ждали 200", got)
	}
	if got := len([]rune(txs[0].Spans[0].Description)); got != maxSpanDescription {
		t.Errorf("len(Description) = %d, ждали %d", got, maxSpanDescription)
	}
}

// TestMapOTLPMaxSpans — раздутый батч спанов одного трейса обрезается, сама
// транзакция остаётся.
func TestMapOTLPMaxSpans(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	spans := []*tracepb.Span{{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /x",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(start.Add(time.Second)),
	}}
	for i := 0; i < maxSpans+10; i++ {
		id := []byte{byte(i), byte(i >> 8), 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
		spans = append(spans, &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            id,
			ParentSpanId:      rootIDBytes,
			Name:              "child",
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(start.Add(time.Millisecond)),
		})
	}

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil, spans...)}, now)
	if len(txs) != 1 {
		t.Fatalf("транзакций: %d", len(txs))
	}
	if len(txs[0].Spans) != maxSpans {
		t.Fatalf("спанов: %d, ждали %d", len(txs[0].Spans), maxSpans)
	}
}

// --- привязка спанов к СВОЕМУ корню (батч коллектора склеивает сервисы) ---

var (
	feRootID   = []byte{0xf0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01} // корень фронтенда
	feClientID = []byte{0xf0, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02} // его http.client
	blRootID   = []byte{0xb0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01} // SERVER-корень биллинга
	blDBID     = []byte{0xb0, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02} // его db-спан
)

// TestMapOTLPMultiServiceBatch — batch-процессор коллектора ШТАТНО склеивает
// ResourceSpans разных сервисов в один экспорт, и трейс, прошедший через два
// сервиса, приезжает с ДВУМЯ корнями (SERVER-спан второго сервиса — корень по
// правилу kind). Каждый спан обязан уехать в СВОЮ транзакцию: SpanWriter копирует
// в строку спана transaction и environment владеющей транзакции, и привязка к
// первому корню трейса заставила бы фильтр по окружению врать.
func TestMapOTLPMultiServiceBatch(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(time.Second)

	frontend := resSpans([]*commonpb.KeyValue{
		strAttr("service.name", "frontend"),
		strAttr("deployment.environment", "prod"),
	},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            feRootID,
			Name:              "GET /a",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            feClientID,
			ParentSpanId:      feRootID,
			Name:              "POST /charge",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
			Attributes: []*commonpb.KeyValue{
				strAttr("http.request.method", "POST"),
				strAttr("url.full", "https://billing/charge"),
			},
		},
	)
	billing := resSpans([]*commonpb.KeyValue{
		strAttr("service.name", "billing"),
		strAttr("deployment.environment", "staging"),
	},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            blRootID,
			ParentSpanId:      feClientID, // родитель в ДРУГОМ сервисе, но SERVER — корень
			Name:              "POST /charge",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            blDBID,
			ParentSpanId:      blRootID,
			Name:              "SELECT accounts",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
			Attributes: []*commonpb.KeyValue{
				strAttr("db.system", "postgresql"),
				strAttr("db.statement", "SELECT * FROM accounts"),
			},
		},
	)

	txs := MapOTLP([]*tracepb.ResourceSpans{frontend, billing}, now)
	if len(txs) != 2 {
		t.Fatalf("транзакций: %d, ждали 2 (по корню на сервис)", len(txs))
	}

	byService := map[string]trace.Transaction{}
	for _, tx := range txs {
		byService[tx.ServerName] = tx
	}
	fe, ok := byService["frontend"]
	if !ok {
		t.Fatalf("нет транзакции frontend: %+v", txs)
	}
	bl, ok := byService["billing"]
	if !ok {
		t.Fatalf("нет транзакции billing: %+v", txs)
	}

	if fe.Environment != "prod" || bl.Environment != "staging" {
		t.Errorf("environment: frontend=%q billing=%q", fe.Environment, bl.Environment)
	}
	if len(fe.Spans) != 1 || fe.Spans[0].SpanID != hex.EncodeToString(feClientID) {
		t.Fatalf("спаны frontend = %+v, ждали только его http.client", fe.Spans)
	}
	if len(bl.Spans) != 1 || bl.Spans[0].SpanID != hex.EncodeToString(blDBID) {
		t.Fatalf("спаны billing = %+v, ждали его собственный db-спан", bl.Spans)
	}
	if bl.Spans[0].Op != "db" {
		t.Errorf("billing db op = %q", bl.Spans[0].Op)
	}
}

// TestMapOTLPDeepChainToOwnRoot — ребёнок цепляется к ближайшему корню ВВЕРХ по
// цепочке parent_span_id, даже если между ними есть промежуточные спаны.
func TestMapOTLPDeepChainToOwnRoot(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(time.Second)
	span := func(id, parent []byte, kind tracepb.Span_SpanKind, name string) *tracepb.Span {
		return &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            id,
			ParentSpanId:      parent,
			Name:              name,
			Kind:              kind,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		}
	}
	midID := []byte{0xb0, 0x03, 0x03, 0x03, 0x03, 0x03, 0x03, 0x03}

	txs := MapOTLP([]*tracepb.ResourceSpans{
		resSpans([]*commonpb.KeyValue{strAttr("service.name", "frontend")},
			span(feRootID, nil, tracepb.Span_SPAN_KIND_SERVER, "GET /a")),
		resSpans([]*commonpb.KeyValue{strAttr("service.name", "billing")},
			span(blRootID, feRootID, tracepb.Span_SPAN_KIND_SERVER, "POST /charge"),
			span(midID, blRootID, tracepb.Span_SPAN_KIND_INTERNAL, "handler"),
			span(blDBID, midID, tracepb.Span_SPAN_KIND_CLIENT, "SELECT accounts"),
		),
	}, now)

	if len(txs) != 2 {
		t.Fatalf("транзакций: %d, ждали 2", len(txs))
	}
	for _, tx := range txs {
		switch tx.ServerName {
		case "frontend":
			if len(tx.Spans) != 0 {
				t.Errorf("спаны frontend = %+v, ждали 0", tx.Spans)
			}
		case "billing":
			if len(tx.Spans) != 2 {
				t.Fatalf("спанов billing: %d, ждали 2 (handler + db)", len(tx.Spans))
			}
		}
	}
}

// TestMapOTLPParentCycle — битый батч с циклом в parent_span_id не должен
// зациклить подъём к корню (кап maxParentHops).
func TestMapOTLPParentCycle(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(time.Second)
	span := func(id, parent []byte, name string) *tracepb.Span {
		return &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            id,
			ParentSpanId:      parent,
			Name:              name,
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		}
	}

	done := make(chan []trace.Transaction, 1)
	go func() {
		done <- MapOTLP([]*tracepb.ResourceSpans{resSpans(nil,
			&tracepb.Span{
				TraceId:           traceIDBytes,
				SpanId:            rootIDBytes,
				Name:              "GET /x",
				Kind:              tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: nanos(start),
				EndTimeUnixNano:   nanos(end),
			},
			span(dbIDBytes, httpIDBytes, "a"), // a → b
			span(httpIDBytes, dbIDBytes, "b"), // b → a: цикл
		)}, now)
	}()
	select {
	case txs := <-done:
		if len(txs) != 1 {
			t.Fatalf("транзакций: %d, ждали 1", len(txs))
		}
		if len(txs[0].Spans) != 2 {
			t.Errorf("спанов: %d, ждали 2 (цикл → запасной корень трейса)", len(txs[0].Spans))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MapOTLP зациклился на цикле parent_span_id")
	}
}

// TestMapOTLPChildBeforeRoot — коллектор НЕ гарантирует порядок спанов в батче:
// ребёнок, приехавший ДО своего корня, обязан к нему привязаться (два прохода —
// не украшение, а требование).
func TestMapOTLPChildBeforeRoot(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(time.Second)

	child := &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            dbIDBytes,
		ParentSpanId:      rootIDBytes,
		Name:              "SELECT orders",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(end),
		Attributes:        []*commonpb.KeyValue{strAttr("db.system", "postgresql")},
	}
	root := &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /checkout",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(end),
	}

	// Ребёнок ПЕРВЫМ в списке.
	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil, child, root)}, now)
	if len(txs) != 1 {
		t.Fatalf("транзакций: %d, ждали 1", len(txs))
	}
	if len(txs[0].Spans) != 1 {
		t.Fatalf("спанов: %d, ждали 1 (ребёнок до корня всё равно привязан)", len(txs[0].Spans))
	}
	if txs[0].Spans[0].SpanID != hex.EncodeToString(dbIDBytes) {
		t.Errorf("SpanID = %q", txs[0].Spans[0].SpanID)
	}
}

// TestMapOTLPOrphanOfOtherTrace — сирота ЧУЖОГО трейса (его корень не приехал)
// отбрасывается и не липнет к корню другого трейса, приехавшего тем же запросом.
func TestMapOTLPOrphanOfOtherTrace(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	end := start.Add(time.Second)
	otherTrace := []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x00, 0x01,
		0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil,
		&tracepb.Span{ // корень трейса A
			TraceId:           traceIDBytes,
			SpanId:            rootIDBytes,
			Name:              "GET /a",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		},
		&tracepb.Span{ // законный ребёнок трейса A
			TraceId:           traceIDBytes,
			SpanId:            dbIDBytes,
			ParentSpanId:      rootIDBytes,
			Name:              "SELECT orders",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		},
		&tracepb.Span{ // сирота трейса B: его корня в запросе нет
			TraceId:           otherTrace,
			SpanId:            httpIDBytes,
			ParentSpanId:      []byte{0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08},
			Name:              "orphan",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(end),
		},
	)}, now)

	if len(txs) != 1 {
		t.Fatalf("транзакций: %d, ждали 1 (у трейса B корня нет)", len(txs))
	}
	if txs[0].TraceID != hex.EncodeToString(traceIDBytes) {
		t.Fatalf("TraceID = %q", txs[0].TraceID)
	}
	if len(txs[0].Spans) != 1 {
		t.Fatalf("спанов: %d, ждали 1 (сирота чужого трейса отброшен, свой спан остался)",
			len(txs[0].Spans))
	}
	if txs[0].Spans[0].SpanID != hex.EncodeToString(dbIDBytes) {
		t.Errorf("к транзакции прилип чужой спан: %q", txs[0].Spans[0].SpanID)
	}
}

// TestMapOTLPMaxDataKeys — число ключей в Span.Data ограничено (Data целиком
// уезжает в колонку `data`), тот же кап, что у тегов.
func TestMapOTLPMaxDataKeys(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	attrs := make([]*commonpb.KeyValue, 0, 1000)
	for i := 0; i < 1000; i++ {
		attrs = append(attrs, strAttr("attr."+strconv.Itoa(i), "v"))
	}

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil,
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            rootIDBytes,
			Name:              "GET /x",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(start.Add(time.Second)),
		},
		&tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            dbIDBytes,
			ParentSpanId:      rootIDBytes,
			Name:              "fat",
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: nanos(start),
			EndTimeUnixNano:   nanos(start.Add(time.Second)),
			Attributes:        attrs,
		},
	)}, now)

	if len(txs) != 1 || len(txs[0].Spans) != 1 {
		t.Fatalf("txs = %+v", txs)
	}
	if got := len(txs[0].Spans[0].Data); got != maxDataKeys {
		t.Fatalf("ключей в Data: %d, ждали кап %d", got, maxDataKeys)
	}
	// Кап тегов корня — тот же (см. capTags), проверяем заодно.
	if got := len(txs[0].Tags); got > 64 {
		t.Errorf("тегов: %d, ждали не больше 64", got)
	}
}

// --- OTLP/JSON: идентификаторы — HEX-строки, а не base64 ---

// TestOTLPUnmarshalJSONHexIDs — спека OTLP отступает от стандартного
// protobuf-JSON ровно здесь: trace_id/span_id/parent_span_id в OTLP/JSON это
// HEX. protojson молча декодирует их как base64 (каждый hex-символ входит в
// base64-алфавит, 32/16 символов кратны 4) и выдаёт мусор нужной ФОРМЫ — 200 OK,
// строки в CH, ни одной записи в лог. Тело настоящего коллектора (OTel-JS шлёт
// JSON по умолчанию) обязано доехать с ТЕМИ ЖЕ id.
func TestOTLPUnmarshalJSONHexIDs(t *testing.T) {
	const (
		wantTrace  = "ab0102030405060708090a0b0c0d0eff"
		wantSpan   = "deadbeef01020304"
		wantParent = "1122334455667788"
	)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "hex-идентификаторы (так шлёт коллектор)",
			body: `{"resourceSpans":[{"scopeSpans":[{"spans":[
				{"traceId":"ab0102030405060708090a0b0c0d0eff",
				 "spanId":"deadbeef01020304",
				 "parentSpanId":"1122334455667788",
				 "name":"GET /checkout","kind":2,
				 "startTimeUnixNano":"1752494400000000000",
				 "endTimeUnixNano":"1752494400500000000"}]}]}]}`,
		},
		{
			name: "написание snake_case (protojson принимает оба)",
			body: `{"resource_spans":[{"scope_spans":[{"spans":[
				{"trace_id":"ab0102030405060708090a0b0c0d0eff",
				 "span_id":"deadbeef01020304",
				 "parent_span_id":"1122334455667788",
				 "name":"GET /checkout","kind":2,
				 "start_time_unix_nano":"1752494400000000000",
				 "end_time_unix_nano":"1752494400500000000"}]}]}]}`,
		},
		{
			name: "base64-идентификаторы всё ещё принимаются (терпимость)",
			body: `{"resourceSpans":[{"scopeSpans":[{"spans":[
				{"traceId":"qwECAwQFBgcICQoLDA0O/w==",
				 "spanId":"3q2+7wECAwQ=",
				 "parentSpanId":"ESIzRFVmd4g=",
				 "name":"GET /checkout","kind":2,
				 "startTimeUnixNano":"1752494400000000000",
				 "endTimeUnixNano":"1752494400500000000"}]}]}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req tracepb.TracesData
			if err := otlpUnmarshal(otlpJSON, []byte(tt.body), &req); err != nil {
				t.Fatalf("otlpUnmarshal: %v", err)
			}
			spans := req.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			if len(spans) != 1 {
				t.Fatalf("спанов: %d", len(spans))
			}
			s := spans[0]
			if got := hex.EncodeToString(s.GetTraceId()); got != wantTrace {
				t.Errorf("trace_id = %q, ждали %q", got, wantTrace)
			}
			if got := hex.EncodeToString(s.GetSpanId()); got != wantSpan {
				t.Errorf("span_id = %q, ждали %q", got, wantSpan)
			}
			if got := hex.EncodeToString(s.GetParentSpanId()); got != wantParent {
				t.Errorf("parent_span_id = %q, ждали %q", got, wantParent)
			}
			if s.GetStartTimeUnixNano() != 1752494400000000000 {
				t.Errorf("start = %d: наносекунды не должны портиться перекодировкой тела",
					s.GetStartTimeUnixNano())
			}
			if s.GetName() != "GET /checkout" {
				t.Errorf("name = %q", s.GetName())
			}
		})
	}
}

// TestOTLPUnmarshalJSONKeepsAttributes — переписывая id, тело пересобирается
// целиком: атрибуты, события и большие числовые значения обязаны пережить это
// без потерь (json.Number, а не float64: иначе наносекунды уехали бы в
// экспоненциальную запись, которую protojson не примет).
func TestOTLPUnmarshalJSONKeepsAttributes(t *testing.T) {
	body := `{"resourceSpans":[{
		"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"billing"}}]},
		"scopeSpans":[{"spans":[
			{"traceId":"ab0102030405060708090a0b0c0d0eff",
			 "spanId":"deadbeef01020304",
			 "name":"GET /checkout","kind":2,
			 "startTimeUnixNano":"1752494400000000000",
			 "endTimeUnixNano":"1752494400500000000",
			 "attributes":[
				{"key":"http.request.method","value":{"stringValue":"GET"}},
				{"key":"db.rows","value":{"intValue":"9007199254740993"}},
				{"key":"trace_id","value":{"stringValue":"ab0102030405060708090a0b0c0d0eff"}}
			 ],
			 "events":[{"name":"exception","timeUnixNano":"1752494400100000000",
				"attributes":[{"key":"exception.type","value":{"stringValue":"TimeoutError"}}]}],
			 "status":{"code":2}}]}]}]}`

	var req tracepb.TracesData
	if err := otlpUnmarshal(otlpJSON, []byte(body), &req); err != nil {
		t.Fatalf("otlpUnmarshal: %v", err)
	}
	txs := MapOTLP(req.GetResourceSpans(), time.Unix(0, 1752494400500000000).UTC())
	if len(txs) != 1 {
		t.Fatalf("транзакций: %d", len(txs))
	}
	tx := txs[0]
	if tx.TraceID != "ab0102030405060708090a0b0c0d0eff" || tx.SpanID != "deadbeef01020304" {
		t.Fatalf("id транзакции: trace=%q span=%q", tx.TraceID, tx.SpanID)
	}
	if tx.ServerName != "billing" {
		t.Errorf("ServerName = %q", tx.ServerName)
	}
	if tx.Status != "internal_error" {
		t.Errorf("Status = %q", tx.Status)
	}
	if got := tx.Tags["db.rows"]; got != "9007199254740993" {
		t.Errorf("Tags[db.rows] = %q: int64 не должен терять точность", got)
	}
	// Атрибут с ключом "trace_id" — это ЗНАЧЕНИЕ поля key, а не имя поля:
	// переписывать его нельзя.
	if got := tx.Tags["trace_id"]; got != "ab0102030405060708090a0b0c0d0eff" {
		t.Errorf("Tags[trace_id] = %q: атрибут не должен быть перекодирован", got)
	}
}

// TestOTLPUnmarshalJSONBadIDsPassThrough — правило переписывания намеренно
// узкое: конвертируется только hex РОВНО нужной длины. Всё остальное уходит в
// protojson нетронутым — и решение (принять как base64 или отбить весь батч)
// остаётся за ним, мы чужих ошибок не глотаем и своих не выдумываем.
func TestOTLPUnmarshalJSONBadIDsPassThrough(t *testing.T) {
	var req tracepb.TracesData

	// Не hex и не base64 → ошибка приходит ОТ protojson, а не теряется у нас.
	garbage := `{"resourceSpans":[{"scopeSpans":[{"spans":[
		{"traceId":"!!!! не base64 и не hex !!!!","spanId":"deadbeef01020304"}]}]}]}`
	if err := otlpUnmarshal(otlpJSON, []byte(garbage), &req); err == nil {
		t.Error("мусорный id: ждали ошибку protojson")
	}

	// Валидный base64 неподходящей под hex длины (3 байта) — не наш случай:
	// пропускаем как есть, protojson декодирует его как base64.
	shortB64 := `{"resourceSpans":[{"scopeSpans":[{"spans":[
		{"traceId":"ab01","spanId":"deadbeef01020304",
		 "startTimeUnixNano":"1752494400000000000"}]}]}]}`
	req.Reset()
	if err := otlpUnmarshal(otlpJSON, []byte(shortB64), &req); err != nil {
		t.Fatalf("короткий base64-id: %v", err)
	}
	s := req.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	if got := len(s.GetTraceId()); got != 3 {
		t.Errorf("len(trace_id) = %d, ждали 3 байта (base64 as-is)", got)
	}
	// span_id при этом всё равно разобран как hex: правило работает по полю.
	if got := hex.EncodeToString(s.GetSpanId()); got != "deadbeef01020304" {
		t.Errorf("span_id = %q", got)
	}
	// А в модель такой trace_id всё равно не попадёт целым — MapOTLP работает
	// с тем, что приехало, и битый трейс просто не сойдётся с другими SDK.

	broken := `{"resourceSpans":[{"scopeSpans":`
	req.Reset()
	if err := otlpUnmarshal(otlpJSON, []byte(broken), &req); err == nil {
		t.Error("битый JSON: ждали ошибку разбора")
	}
}

// СОВРЕМЕННАЯ семконвенция OTel: db.system.name + db.query.text (старые
// db.system/db.statement SDK уже не шлют). Без чтения нового ключа такой спан
// получал бы op `client` (по kind), и НИ ОДИН детектор его не видел бы:
// hasOpPrefix(op, "db") ложен — ни N+1, ни медленных запросов у Postgres/MySQL.
func TestMapOTLPModernDBSemconv(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	const sel = "SELECT * FROM users WHERE id = 1"
	spans := []*tracepb.Span{{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /feed",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(start.Add(2 * time.Second)),
	}}
	// Шесть одинаковых запросов по 10мс — N+1 (порог 5 штук и 20мс).
	for i := 0; i < 6; i++ {
		spans = append(spans, &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x00, byte(i + 1)},
			ParentSpanId:      rootIDBytes,
			Name:              "SELECT users",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start.Add(time.Duration(i*10) * time.Millisecond)),
			EndTimeUnixNano:   nanos(start.Add(time.Duration(i*10+10) * time.Millisecond)),
			Attributes: []*commonpb.KeyValue{
				strAttr("db.system.name", "postgresql"),
				strAttr("db.query.text", sel),
			},
		})
	}
	// И один запрос на 600мс — медленный (порог 500мс).
	spans = append(spans, &tracepb.Span{
		TraceId:           traceIDBytes,
		SpanId:            []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x00, 0x20},
		ParentSpanId:      rootIDBytes,
		Name:              "SELECT orders",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: nanos(start.Add(time.Second)),
		EndTimeUnixNano:   nanos(start.Add(1600 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			strAttr("db.system.name", "postgresql"),
			strAttr("db.query.text", "SELECT * FROM orders"),
		},
	})

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil, spans...)}, now)
	if len(txs) != 1 {
		t.Fatalf("транзакций %d, want 1", len(txs))
	}
	if got, want := txs[0].Spans[0].Op, "db"; got != want {
		t.Fatalf("op спана с db.system.name = %q, want %q", got, want)
	}
	if got := txs[0].Spans[0].Description; got != sel {
		t.Fatalf("описание спана = %q, want %q", got, sel)
	}

	found := trace.Detect(txs[0], trace.DefaultDetectorConfig())
	kinds := map[string]bool{}
	for _, f := range found {
		kinds[f.Kind] = true
	}
	if !kinds[trace.KindNPlusOne] || !kinds[trace.KindSlowDBQuery] {
		t.Fatalf("находки = %+v, want n_plus_one и slow_db_query", found)
	}
}

// Redis по современной семконвенции (db.system.name=redis) обязан получать тот
// же op db.redis, что и по старой: иначе его команда уедет в SQL-нормализатор.
func TestMapOTLPModernRedisSemconv(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	spans := []*tracepb.Span{{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /feed",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(start.Add(200 * time.Millisecond)),
	}, {
		TraceId:           traceIDBytes,
		SpanId:            dbIDBytes,
		ParentSpanId:      rootIDBytes,
		Name:              "GET",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(start.Add(5 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			strAttr("db.system.name", "redis"),
			strAttr("db.query.text", "GET user:42"),
		},
	}}

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil, spans...)}, now)
	if len(txs) != 1 || len(txs[0].Spans) != 1 {
		t.Fatalf("транзакции = %+v", txs)
	}
	if got, want := txs[0].Spans[0].Op, "db.redis"; got != want {
		t.Fatalf("op = %q, want %q", got, want)
	}
	if got, want := txs[0].Spans[0].Description, "GET user:42"; got != want {
		t.Fatalf("описание = %q, want %q", got, want)
	}
}

// Redis-цикл, приехавший по OTLP, обязан детектиться так же, как через Sentry
// SDK: db.system=redis получает op `db.redis`, и его описание нормализуется как
// ключ кеша, а не как SQL (в SQL `:42` — именованный плейсхолдер, и все ключи
// остались бы разными).
func TestMapOTLPRedisSpansDetectAsNPlusOne(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)

	spans := []*tracepb.Span{{
		TraceId:           traceIDBytes,
		SpanId:            rootIDBytes,
		Name:              "GET /feed",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(start.Add(200 * time.Millisecond)),
	}}
	for i := 0; i < 8; i++ {
		id := []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x00, byte(i + 1)}
		spans = append(spans, &tracepb.Span{
			TraceId:           traceIDBytes,
			SpanId:            id,
			ParentSpanId:      rootIDBytes,
			Name:              "GET",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(start.Add(time.Duration(i*5) * time.Millisecond)),
			EndTimeUnixNano:   nanos(start.Add(time.Duration(i*5+5) * time.Millisecond)),
			Attributes: []*commonpb.KeyValue{
				strAttr("db.system", "redis"),
				strAttr("db.statement", "GET user:"+strconv.Itoa(i)),
			},
		})
	}

	txs := MapOTLP([]*tracepb.ResourceSpans{resSpans(nil, spans...)}, now)
	if len(txs) != 1 {
		t.Fatalf("транзакций %d, want 1", len(txs))
	}
	if got, want := txs[0].Spans[0].Op, "db.redis"; got != want {
		t.Fatalf("op redis-спана = %q, want %q", got, want)
	}
	if got, want := txs[0].Spans[0].Description, "GET user:0"; got != want {
		t.Fatalf("описание redis-спана = %q, want %q", got, want)
	}

	found := trace.Detect(txs[0], trace.DefaultDetectorConfig())
	if len(found) != 1 || found[0].Kind != trace.KindNPlusOne {
		t.Fatalf("находки = %+v, want один n_plus_one", found)
	}
	if want := "GET user:?"; found[0].Description != want {
		t.Errorf("Description = %q, want %q", found[0].Description, want)
	}
}
