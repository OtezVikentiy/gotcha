package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// txBase — момент, от которого строятся timestamp'ы фикстур. Абсолютной даты в
// фикстуре быть не может: парсер отбрасывает timestamp'ы вне окна хранения
// (см. ingest.ErrTimestampOutOfWindow), и вшитая константа однажды выпала бы за
// его границу и «сломала» бы тест сама по себе.
func txBase() time.Time {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
}

// unixFloat — timestamp в том виде, в каком его шлют sentry-php/sentry-go.
func unixFloat(t time.Time) string {
	return fmt.Sprintf("%.6f", float64(t.UnixNano())/1e9)
}

// testTransactionJSON — канонический payload: timestamps unix-float,
// contexts.trace несёт trace_id/span_id/op/status. Транзакция длится 500ms,
// спаны — 100ms и 250ms.
func testTransactionJSON(base time.Time) string {
	at := func(ms int) string { return unixFloat(base.Add(time.Duration(ms) * time.Millisecond)) }
	return fmt.Sprintf(`{
  "event_id":"5c2b2f5b1d1f4f2a9c3f6a7b8c9d0e1f",
  "type":"transaction",
  "transaction":"GET /api/users",
  "start_timestamp":%s,
  "timestamp":%s,
  "environment":"prod",
  "release":"api@1.2.3",
  "server_name":"web-01",
  "user":{"id":"42","email":"u@example.com"},
  "tags":{"http.method":"GET"},
  "contexts":{"trace":{
    "trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "span_id":"bbbbbbbbbbbbbbbb",
    "op":"http.server",
    "status":"ok"
  }},
  "spans":[
    {"span_id":"cccccccccccccccc","parent_span_id":"bbbbbbbbbbbbbbbb",
     "op":"db.sql.query","description":"SELECT * FROM users WHERE id = 1",
     "start_timestamp":%s,"timestamp":%s,
     "status":"ok","data":{"db.system":"postgresql"}},
    {"span_id":"dddddddddddddddd","parent_span_id":"bbbbbbbbbbbbbbbb",
     "op":"http.client","description":"GET https://billing/health",
     "start_timestamp":%s,"timestamp":%s,
     "status":"ok"}
  ]
}`, at(100), at(600), at(200), at(300), at(300), at(550))
}

// testTransactionRFC3339JSON — тот же payload строками: часть SDK
// (sentry-python/старые sentry-php) шлёт timestamps в RFC3339, а не unix-float.
func testTransactionRFC3339JSON(base time.Time) string {
	at := func(ms int) string {
		return base.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)
	}
	return fmt.Sprintf(`{
  "type":"transaction",
  "transaction":"GET /api/users",
  "start_timestamp":%q,
  "timestamp":%q,
  "contexts":{"trace":{
    "trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "span_id":"bbbbbbbbbbbbbbbb",
    "op":"http.server","status":"ok"
  }},
  "spans":[
    {"span_id":"cccccccccccccccc","parent_span_id":"bbbbbbbbbbbbbbbb",
     "op":"db.sql.query","description":"SELECT 1",
     "start_timestamp":%q,
     "timestamp":%q,"status":"ok"}
  ]
}`, at(100), at(600), at(200), at(300))
}

func TestParseTransactionCanonical(t *testing.T) {
	tx, err := ingest.ParseTransaction([]byte(testTransactionJSON(txBase())))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if tx.Name != "GET /api/users" {
		t.Errorf("Name = %q", tx.Name)
	}
	if tx.TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || tx.SpanID != "bbbbbbbbbbbbbbbb" {
		t.Errorf("ids = %q/%q", tx.TraceID, tx.SpanID)
	}
	if tx.Op != "http.server" || tx.Status != "ok" {
		t.Errorf("op/status = %q/%q", tx.Op, tx.Status)
	}
	if tx.Environment != "prod" || tx.Release != "api@1.2.3" || tx.ServerName != "web-01" {
		t.Errorf("meta = %q/%q/%q", tx.Environment, tx.Release, tx.ServerName)
	}
	if tx.UserID != "42" {
		t.Errorf("UserID = %q", tx.UserID)
	}
	if tx.Tags["http.method"] != "GET" {
		t.Errorf("Tags = %v", tx.Tags)
	}
	if tx.Source != "sentry" {
		t.Errorf("Source = %q, want sentry", tx.Source)
	}
	if got := tx.DurationUS(); got != 500_000 {
		t.Errorf("DurationUS = %d, want 500000", got)
	}
	if len(tx.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(tx.Spans))
	}
	s0 := tx.Spans[0]
	if s0.SpanID != "cccccccccccccccc" || s0.ParentSpanID != "bbbbbbbbbbbbbbbb" {
		t.Errorf("span0 ids = %q/%q", s0.SpanID, s0.ParentSpanID)
	}
	if s0.Op != "db.sql.query" || s0.Description != "SELECT * FROM users WHERE id = 1" {
		t.Errorf("span0 op/desc = %q/%q", s0.Op, s0.Description)
	}
	if got := s0.DurationUS(); got != 100_000 {
		t.Errorf("span0 DurationUS = %d, want 100000", got)
	}
	if s0.Data["db.system"] != "postgresql" {
		t.Errorf("span0 data = %v", s0.Data)
	}
	if got := tx.Spans[1].DurationUS(); got != 250_000 {
		t.Errorf("span1 DurationUS = %d, want 250000", got)
	}
}

func TestParseTransactionRFC3339Timestamps(t *testing.T) {
	base := txBase()
	tx, err := ingest.ParseTransaction([]byte(testTransactionRFC3339JSON(base)))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	want := base.Add(100 * time.Millisecond)
	if !tx.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", tx.Start, want)
	}
	if got := tx.DurationUS(); got != 500_000 {
		t.Errorf("DurationUS = %d, want 500000", got)
	}
	if len(tx.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(tx.Spans))
	}
	if got := tx.Spans[0].DurationUS(); got != 100_000 {
		t.Errorf("span DurationUS = %d, want 100000", got)
	}
}

func TestParseTransactionWithoutTraceContextIsError(t *testing.T) {
	cases := map[string]string{
		"no contexts":     `{"type":"transaction","transaction":"GET /x","timestamp":1752000000.0}`,
		"no trace":        `{"type":"transaction","transaction":"GET /x","contexts":{"os":{"name":"linux"}}}`,
		"empty trace_id":  `{"type":"transaction","transaction":"GET /x","contexts":{"trace":{"trace_id":"","span_id":"bbbbbbbbbbbbbbbb"}}}`,
		"broken json":     `{"type":"transaction",`,
		"empty trace obj": `{"type":"transaction","transaction":"GET /x","contexts":{"trace":{}}}`,
	}
	for name, raw := range cases {
		if _, err := ingest.ParseTransaction([]byte(raw)); err == nil {
			t.Errorf("%s: ParseTransaction returned nil error, want error", name)
		}
	}
}

func TestParseTransactionCapsUntrustedStrings(t *testing.T) {
	raw := fmt.Sprintf(`{"type":"transaction","transaction":%q,
		"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"span_id":"bbbbbbbbbbbbbbbb","op":%q,"status":"ok"}},
		"spans":[{"span_id":"cccccccccccccccc","op":%q,"description":%q}]}`,
		strings.Repeat("n", 500), strings.Repeat("o", 300),
		strings.Repeat("o", 300), strings.Repeat("d", 5000))

	tx, err := ingest.ParseTransaction([]byte(raw))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if n := len([]rune(tx.Name)); n != 200 {
		t.Errorf("len(Name) = %d, want 200", n)
	}
	if n := len([]rune(tx.Op)); n != 100 {
		t.Errorf("len(Op) = %d, want 100", n)
	}
	if len(tx.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(tx.Spans))
	}
	if n := len([]rune(tx.Spans[0].Op)); n != 100 {
		t.Errorf("len(span.Op) = %d, want 100", n)
	}
	if n := len([]rune(tx.Spans[0].Description)); n != 2000 {
		t.Errorf("len(span.Description) = %d, want 2000", n)
	}
}

// TestParseTransactionCapsSpanCount: спанов в одной транзакции не больше 1000 —
// раздутый payload не должен утаскивать в CH десятки тысяч строк. Транзакция при
// этом остаётся: лишние спаны отбрасываются, а не весь item.
func TestParseTransactionCapsSpanCount(t *testing.T) {
	base := txBase()
	var spans []string
	for i := 0; i < 1500; i++ {
		spans = append(spans, fmt.Sprintf(
			`{"span_id":%q,"parent_span_id":"bbbbbbbbbbbbbbbb","op":"db.sql.query",
			  "description":"SELECT 1","start_timestamp":%s,"timestamp":%s,"status":"ok"}`,
			fmt.Sprintf("%016x", i), unixFloat(base), unixFloat(base.Add(time.Millisecond))))
	}
	raw := fmt.Sprintf(`{"type":"transaction","transaction":"GET /x",
		"start_timestamp":%s,"timestamp":%s,
		"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"span_id":"bbbbbbbbbbbbbbbb","op":"http.server","status":"ok"}},
		"spans":[%s]}`, unixFloat(base), unixFloat(base.Add(time.Second)), strings.Join(spans, ","))

	tx, err := ingest.ParseTransaction([]byte(raw))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if len(tx.Spans) != 1000 {
		t.Fatalf("spans = %d, want 1000 (cap)", len(tx.Spans))
	}
	// Каппим ХВОСТ, а не начало: первый спан payload'а должен уцелеть.
	if tx.Spans[0].SpanID != fmt.Sprintf("%016x", 0) {
		t.Errorf("first span = %q, want the first span of the payload", tx.Spans[0].SpanID)
	}
}

// TestParseTransactionNormalizesEmptyStatus: SDK часто опускают status у
// успешных транзакций/спанов, а MV transactions_5m считает провалом всё, что
// != 'ok', — без нормализации "" → "ok" failure rate был бы 100%.
func TestParseTransactionNormalizesEmptyStatus(t *testing.T) {
	base := txBase()
	raw := fmt.Sprintf(`{"type":"transaction","transaction":"GET /x",
		"start_timestamp":%s,"timestamp":%s,
		"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"span_id":"bbbbbbbbbbbbbbbb","op":"http.server"}},
		"spans":[
			{"span_id":"cccccccccccccccc","op":"db.sql.query","description":"SELECT 1",
			 "start_timestamp":%s,"timestamp":%s},
			{"span_id":"dddddddddddddddd","op":"db.sql.query","description":"SELECT 2",
			 "start_timestamp":%s,"timestamp":%s,"status":"internal_error"}
		]}`,
		unixFloat(base), unixFloat(base.Add(time.Second)),
		unixFloat(base), unixFloat(base.Add(10*time.Millisecond)),
		unixFloat(base), unixFloat(base.Add(20*time.Millisecond)))

	tx, err := ingest.ParseTransaction([]byte(raw))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if tx.Status != "ok" {
		t.Errorf("transaction Status = %q, want %q", tx.Status, "ok")
	}
	if len(tx.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(tx.Spans))
	}
	if tx.Spans[0].Status != "ok" {
		t.Errorf("span0 Status = %q, want %q", tx.Spans[0].Status, "ok")
	}
	// Явный статус не трогаем.
	if tx.Spans[1].Status != "internal_error" {
		t.Errorf("span1 Status = %q, want internal_error", tx.Spans[1].Status)
	}
}

// TestParseTransactionLowercasesIDs: trace_id/span_id/parent_span_id хранятся в
// каноническом hex'е нижнего регистра — иначе один и тот же трейс, пришедший из
// Sentry-SDK и (в будущем) из OTLP, разъедется и в семплировании (trace.Keep), и
// в join'е spans↔transactions.
func TestParseTransactionLowercasesIDs(t *testing.T) {
	base := txBase()
	raw := fmt.Sprintf(`{"type":"transaction","transaction":"GET /x",
		"start_timestamp":%s,"timestamp":%s,
		"contexts":{"trace":{"trace_id":"4BF92F3577B34DA6A3CE929D0E0E4736",
			"span_id":"00F067AA0BA902B7","op":"http.server","status":"ok"}},
		"spans":[{"span_id":"AABBCCDDEEFF0011","parent_span_id":"00F067AA0BA902B7",
			"op":"db.sql.query","description":"SELECT 1",
			"start_timestamp":%s,"timestamp":%s,"status":"ok"}]}`,
		unixFloat(base), unixFloat(base.Add(time.Second)),
		unixFloat(base), unixFloat(base.Add(10*time.Millisecond)))

	tx, err := ingest.ParseTransaction([]byte(raw))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if tx.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("TraceID = %q, want lowercase hex", tx.TraceID)
	}
	if tx.SpanID != "00f067aa0ba902b7" {
		t.Errorf("SpanID = %q, want lowercase hex", tx.SpanID)
	}
	if len(tx.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(tx.Spans))
	}
	if tx.Spans[0].SpanID != "aabbccddeeff0011" || tx.Spans[0].ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("span ids = %q/%q, want lowercase hex",
			tx.Spans[0].SpanID, tx.Spans[0].ParentSpanID)
	}
}

// TestParseTransactionRejectsTimestampsOutsideWindow: timestamp вне окна
// хранения не должен доезжать до писателя — ни транзакцией, ни спаном (см.
// ingest.ErrTimestampOutOfWindow и TestTransactionTimestampPoisonDoesNotWedgeWriter).
func TestParseTransactionRejectsTimestampsOutsideWindow(t *testing.T) {
	now := time.Now().UTC()
	tx := func(start time.Time) string {
		return fmt.Sprintf(`{"type":"transaction","transaction":"GET /x",
			"start_timestamp":%s,"timestamp":%s,
			"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"span_id":"bbbbbbbbbbbbbbbb","op":"http.server","status":"ok"}}}`,
			unixFloat(start), unixFloat(start.Add(time.Second)))
	}
	outside := map[string]time.Time{
		"prehistoric":    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		"older than TTL": now.Add(-91 * 24 * time.Hour),
		"far future":     now.Add(48 * time.Hour),
	}
	for name, start := range outside {
		if _, err := ingest.ParseTransaction([]byte(tx(start))); !errors.Is(err, ingest.ErrTimestampOutOfWindow) {
			t.Errorf("%s: err = %v, want ErrTimestampOutOfWindow", name, err)
		}
	}
	// Внутри окна — принимаем.
	for name, start := range map[string]time.Time{
		"just now":       now.Add(-time.Minute),
		"almost TTL":     now.Add(-89 * 24 * time.Hour),
		"slightly ahead": now.Add(time.Hour),
	} {
		if _, err := ingest.ParseTransaction([]byte(tx(start))); err != nil {
			t.Errorf("%s: ParseTransaction = %v, want nil", name, err)
		}
	}

	// Отдельный спан-«отравитель» выкидывается, транзакция остаётся.
	base := txBase()
	raw := fmt.Sprintf(`{"type":"transaction","transaction":"GET /x",
		"start_timestamp":%s,"timestamp":%s,
		"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"span_id":"bbbbbbbbbbbbbbbb","op":"http.server","status":"ok"}},
		"spans":[
			{"span_id":"cccccccccccccccc","op":"db.sql.query","description":"good",
			 "start_timestamp":%s,"timestamp":%s,"status":"ok"},
			{"span_id":"dddddddddddddddd","op":"db.sql.query","description":"poison",
			 "start_timestamp":946684800.0,"timestamp":946684801.0,"status":"ok"}
		]}`,
		unixFloat(base), unixFloat(base.Add(time.Second)),
		unixFloat(base), unixFloat(base.Add(10*time.Millisecond)))
	parsed, err := ingest.ParseTransaction([]byte(raw))
	if err != nil {
		t.Fatalf("ParseTransaction: %v", err)
	}
	if len(parsed.Spans) != 1 || parsed.Spans[0].Description != "good" {
		t.Fatalf("spans = %+v, want only the in-window span", parsed.Spans)
	}
}

// --- интеграция (docker: PG+CH) ---

// freshTransactionJSON — тот же канонический payload, но с timestamp'ами
// «только что»: у CH-таблиц transactions/spans есть TTL (90/30 дней), и строка
// с прошлогодним timestamp'ом отбрасывается прямо на вставке.
// Длительности те же: транзакция 500ms, спан 100ms.
func freshTransactionJSON() string {
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
	  "release":"api@1.2.3",
	  "server_name":"web-01",
	  "user":{"id":"42"},
	  "tags":{"http.method":"GET"},
	  "contexts":{"trace":{
	    "trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	    "span_id":"bbbbbbbbbbbbbbbb",
	    "op":"http.server",
	    "status":"ok"
	  }},
	  "spans":[
	    {"span_id":"cccccccccccccccc","parent_span_id":"bbbbbbbbbbbbbbbb",
	     "op":"db.sql.query","description":"SELECT * FROM users WHERE id = 1",
	     "start_timestamp":%s,"timestamp":%s,
	     "status":"ok","data":{"db.system":"postgresql"}},
	    {"span_id":"dddddddddddddddd","parent_span_id":"bbbbbbbbbbbbbbbb",
	     "op":"http.client","description":"GET https://billing/health",
	     "start_timestamp":%s,"timestamp":%s,"status":"ok"}
	  ]
	}`,
		unix(start), unix(end),
		unix(start.Add(100*time.Millisecond)), unix(start.Add(200*time.Millisecond)),
		unix(start.Add(200*time.Millisecond)), unix(start.Add(450*time.Millisecond)))
}

func transactionEnvelope(payload string) string {
	return "{}\n{\"type\":\"transaction\"}\n" + strings.ReplaceAll(payload, "\n", "") + "\n"
}

// nPlusOneTransactionJSON — транзакция с шестью одинаковыми по структуре
// db-спанами под одним родителем (литералы разные — их схлопнет нормализация):
// канонический N+1, который детекторы обязаны найти при пороге по умолчанию (5).
func nPlusOneTransactionJSON() string {
	end := time.Now().UTC()
	start := end.Add(-500 * time.Millisecond)
	unix := func(t time.Time) string {
		return fmt.Sprintf("%.6f", float64(t.UnixNano())/1e9)
	}
	var spans []string
	for i := 0; i < 6; i++ {
		s := start.Add(time.Duration(10*i) * time.Millisecond)
		spans = append(spans, fmt.Sprintf(
			`{"span_id":"cccccccccccccc%02d","parent_span_id":"bbbbbbbbbbbbbbbb",`+
				`"op":"db.sql.query","description":"SELECT * FROM users WHERE id = %d",`+
				`"start_timestamp":%s,"timestamp":%s,"status":"ok"}`,
			i, 100+i, unix(s), unix(s.Add(5*time.Millisecond))))
	}
	return fmt.Sprintf(`{
	  "event_id":"7d3b2f5b1d1f4f2a9c3f6a7b8c9d0e2a",
	  "type":"transaction",
	  "transaction":"GET /api/users",
	  "start_timestamp":%s,
	  "timestamp":%s,
	  "environment":"prod",
	  "contexts":{"trace":{
	    "trace_id":"cccccccccccccccccccccccccccccccc",
	    "span_id":"bbbbbbbbbbbbbbbb",
	    "op":"http.server",
	    "status":"ok"
	  }},
	  "spans":[%s]
	}`, unix(start), unix(end), strings.Join(spans, ","))
}

// TestTransactionDetectionEndToEnd: envelope с транзакцией из шести одинаковых
// db-спанов → в PG появляется проблема n_plus_one, а спаны всё равно уезжают в CH.
func TestTransactionDetectionEndToEnd(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, transactionEnvelope(nPlusOneTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	var kind, title, culprit, status, sampleTraceID string
	var count int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := s.pool.QueryRow(ctx,
			"SELECT kind, title, culprit, status, count, sample_trace_id FROM perf_issues WHERE project_id = $1",
			s.project.ID).Scan(&kind, &title, &culprit, &status, &count, &sampleTraceID)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if kind != trace.KindNPlusOne || count != 1 || status != "unresolved" ||
		culprit != "GET /api/users" || sampleTraceID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("perf_issue: kind=%q title=%q culprit=%q status=%q count=%d trace=%q",
			kind, title, culprit, status, count, sampleTraceID)
	}
	if !strings.Contains(title, "N+1") {
		t.Errorf("title = %q, want N+1 prefix", title)
	}

	// Корневой спан + 6 дочерних: детекция не мешает записи в CH.
	pid := uint64(s.project.ID)
	if got := waitCH(t, s, "SELECT count(*) FROM spans WHERE project_id = ?", []any{pid}, 7); got != 7 {
		t.Fatalf("spans rows = %d, want 7", got)
	}
}

// waitCH поллит скалярный запрос, пока не получит want (батчер флашится ≤5s).
func waitCH(t *testing.T, s *stack, query string, args []any, want uint64) uint64 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(25 * time.Second)
	var got uint64
	for time.Now().Before(deadline) {
		if err := s.ch.QueryRow(ctx, query, args...).Scan(&got); err == nil && got == want {
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
	return got
}

func TestTransactionEnvelopeEndToEnd(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	pid := uint64(s.project.ID)
	if got := waitCH(t, s, "SELECT count(*) FROM transactions WHERE project_id = ?", []any{pid}, 1); got != 1 {
		t.Fatalf("transactions rows = %d, want 1", got)
	}
	// Корневой спан + 2 дочерних.
	if got := waitCH(t, s, "SELECT count(*) FROM spans WHERE project_id = ?", []any{pid}, 3); got != 3 {
		t.Fatalf("spans rows = %d, want 3", got)
	}

	ctx := context.Background()
	var name, traceID, environment string
	var durationUS uint32
	if err := s.ch.QueryRow(ctx,
		"SELECT transaction, trace_id, environment, duration_us FROM transactions WHERE project_id = ?", pid).
		Scan(&name, &traceID, &environment, &durationUS); err != nil {
		t.Fatalf("transaction row: %v", err)
	}
	if name != "GET /api/users" || traceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		environment != "prod" || durationUS != 500_000 {
		t.Errorf("row = %q/%q/%q/%d", name, traceID, environment, durationUS)
	}
}

// TestTransactionSampleRateZeroWritesNothing: при transaction_sample_rate=0
// запрос принимается (200), но в CH не попадает НИЧЕГО.
func TestTransactionSampleRateZeroWritesNothing(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = 0 WHERE id = $1", s.project.ID); err != nil {
		t.Fatalf("set sample rate: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Дать пайплайну и батчеру заведомо больше времени, чем нужно на запись.
	time.Sleep(2 * time.Second)
	s.pipeline.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.spans.Close(cctx); err != nil {
		t.Fatalf("span writer close: %v", err)
	}

	pid := uint64(s.project.ID)
	var txCount, spanCount uint64
	if err := s.ch.QueryRow(ctx, "SELECT count(*) FROM transactions WHERE project_id = ?", pid).Scan(&txCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if err := s.ch.QueryRow(ctx, "SELECT count(*) FROM spans WHERE project_id = ?", pid).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if txCount != 0 || spanCount != 0 {
		t.Fatalf("sample_rate=0: transactions=%d spans=%d, want 0/0", txCount, spanCount)
	}
}

// TestTransactionQuotaExhaustedStillAcceptsErrors — половина инварианта
// «квоты независимы»: транзакции упёрлись в свою квоту (429), а ошибки тем же
// DSN-ключом по-прежнему принимаются.
func TestTransactionQuotaExhaustedStillAcceptsErrors(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetTransactionQuota(ctx, s.org.ID, 2); err != nil {
		t.Fatalf("SetTransactionQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	for i := 0; i < 2; i++ {
		resp := s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("transaction %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
	resp := s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third transaction: status = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing")
	}

	// Квота ошибок не тронута — событие тем же ключом принимается.
	resp = s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("error event after transaction quota exhausted: status = %d, want 200", resp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)

	// Счётчики в org_usage раздельные: транзакции не тратили бюджет ошибок.
	txUsed, err := s.orgSvc.TransactionUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("TransactionUsage: %v", err)
	}
	if txUsed != 3 { // третья посчитана, но отбита
		t.Errorf("TransactionUsage = %d, want 3", txUsed)
	}
	evUsed, err := s.orgSvc.Usage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if evUsed != 1 {
		t.Errorf("Usage = %d, want 1", evUsed)
	}
}

// TestEventQuotaExhaustedStillAcceptsTransactions — вторая половина
// инварианта: ошибки упёрлись в свою квоту, транзакции принимаются.
func TestEventQuotaExhaustedStillAcceptsTransactions(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetQuota(ctx, s.org.ID, 1); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first error: status = %d, want 200", resp.StatusCode)
	}
	resp = s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second error: status = %d, want 429", resp.StatusCode)
	}

	resp = s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transaction after event quota exhausted: status = %d, want 200", resp.StatusCode)
	}
	pid := uint64(s.project.ID)
	if got := waitCH(t, s, "SELECT count(*) FROM transactions WHERE project_id = ?", []any{pid}, 1); got != 1 {
		t.Fatalf("transactions rows = %d, want 1", got)
	}
}

// poisonEnvelope — envelope из n транзакций, чьи timestamp'ы разнесены по n
// РАЗНЫМ месяцам (2000-01, 2000-02, ...).
func poisonEnvelope(n int) string {
	var b strings.Builder
	b.WriteString("{}\n")
	for i := 0; i < n; i++ {
		start := time.Date(2000+i/12, time.Month(i%12+1), 15, 12, 0, 0, 0, time.UTC)
		item := fmt.Sprintf(`{"type":"transaction","transaction":"GET /poison",`+
			`"start_timestamp":%s,"timestamp":%s,`+
			`"contexts":{"trace":{"trace_id":%q,"span_id":%q,"op":"http.server","status":"ok"}},`+
			`"spans":[{"span_id":%q,"parent_span_id":%q,"op":"db.sql.query","description":"SELECT 1",`+
			`"start_timestamp":%s,"timestamp":%s,"status":"ok"}]}`,
			unixFloat(start), unixFloat(start.Add(time.Second)),
			fmt.Sprintf("%032x", i), fmt.Sprintf("%016x", i),
			fmt.Sprintf("%016x", i+1_000_000), fmt.Sprintf("%016x", i),
			unixFloat(start), unixFloat(start.Add(100*time.Millisecond)))
		b.WriteString("{\"type\":\"transaction\"}\n" + item + "\n")
	}
	return b.String()
}

// TestTransactionTimestampPoisonDoesNotWedgeWriter — регрессия на partition
// poison pill. Публичный DSN-ключ по замыслу лежит внутри клиентского
// приложения, так что кто угодно может прислать envelope с сотнями транзакций,
// чьи timestamp'ы попадают в сотни разных МЕСЯЦЕВ. Таблицы transactions/spans
// партиционированы по toYYYYMM(timestamp), а ClickHouse отбивает INSERT-блок
// больше чем в 100 партиций («Code: 252 ... Too many partitions for single
// INSERT block»): такая пачка падала бы на вставке, возвращалась в голову
// буфера и вставала бы намертво — трейсинг переставал бы писаться для ВСЕГО
// инстанса, для всех организаций. Теперь такие item'ы отбрасываются на
// парсинге, и следующая нормальная транзакция доезжает в CH.
func TestTransactionTimestampPoisonDoesNotWedgeWriter(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, poisonEnvelope(200), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poison envelope: status = %d, want 200", resp.StatusCode)
	}

	// Нормальная транзакция ПОСЛЕ отравы: если писатель заклинило, она навсегда
	// останется за отравленной пачкой в буфере и в CH не появится.
	resp = s.post(t, path, transactionEnvelope(freshTransactionJSON()), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good envelope: status = %d, want 200", resp.StatusCode)
	}

	pid := uint64(s.project.ID)
	const q = "SELECT count(*) FROM transactions WHERE project_id = ? AND transaction = 'GET /api/users'"
	if got := waitCH(t, s, q, []any{pid}, 1); got != 1 {
		t.Fatalf("good transaction rows = %d, want 1 (span writer wedged by the poison batch?)", got)
	}

	// Сама отрава в CH не попала — её отбросили ещё на парсинге.
	var poison uint64
	if err := s.ch.QueryRow(context.Background(),
		"SELECT count(*) FROM transactions WHERE project_id = ? AND transaction = 'GET /poison'",
		pid).Scan(&poison); err != nil {
		t.Fatalf("count poison: %v", err)
	}
	if poison != 0 {
		t.Errorf("poison transactions in CH = %d, want 0", poison)
	}
	// И буфер не пришлось спасать переполнением: ничего не выброшено.
	if dropped := s.spans.Dropped(); dropped != 0 {
		t.Errorf("span writer Dropped() = %d, want 0", dropped)
	}
}

// TestEventTraceIDGoesToClickHouse: событие с contexts.trace → колонки
// events.trace_id/span_id заполнены.
func TestEventTraceIDGoesToClickHouse(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	const ev = `{"event_id":"1ec79c33ec9942ab8353589fcb2e04dc","level":"error",
	"message":"traced boom",
	"contexts":{"trace":{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","span_id":"bbbbbbbbbbbbbbbb","op":"http.server"}}}`

	resp := s.post(t, path, envelopeBody(ev), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)

	pid := uint64(s.project.ID)
	if got := waitCH(t, s, "SELECT count(*) FROM events WHERE project_id = ? AND trace_id != ''", []any{pid}, 1); got != 1 {
		t.Fatalf("events with trace_id = %d, want 1", got)
	}
	var traceID, spanID string
	if err := s.ch.QueryRow(context.Background(),
		"SELECT trace_id, span_id FROM events WHERE project_id = ?", pid).Scan(&traceID, &spanID); err != nil {
		t.Fatalf("event row: %v", err)
	}
	if traceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || spanID != "bbbbbbbbbbbbbbbb" {
		t.Errorf("trace_id/span_id = %q/%q", traceID, spanID)
	}
}
