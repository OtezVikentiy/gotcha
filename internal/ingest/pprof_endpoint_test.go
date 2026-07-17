package ingest_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"

	pp "github.com/google/pprof/profile"
)

func pprofBody(t *testing.T) []byte {
	t.Helper()
	fn := &pp.Function{ID: 1, Name: "main", Filename: "m.go"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
		Sample:     []*pp.Sample{{Location: []*pp.Location{loc}, Value: []int64{5}}},
	}
	var buf bytes.Buffer
	if err := prof.Write(&buf); err != nil {
		t.Fatalf("write pprof: %v", err)
	}
	return buf.Bytes()
}

func (s *stack) postPprof(t *testing.T, body []byte, query, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", s.srv.URL+"/profiles/pprof"+query, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPprofEndpoint(t *testing.T) {
	s := newStack(t)
	body := pprofBody(t)

	// Валидный pprof + метаданные → 202, профиль с Service/TraceID из query.
	resp := s.postPprof(t, body, "?service=api&type=samples&trace_id=tr-99", s.key.PublicKey)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if s.profiles.count() != 1 {
		t.Fatalf("sink got %d, want 1", s.profiles.count())
	}
	if s.profiles.pros[0].Service != "api" {
		t.Fatalf("service = %q, want api", s.profiles.pros[0].Service)
	}
	if s.profiles.pros[0].TraceID != "tr-99" {
		t.Fatalf("trace_id = %q, want tr-99", s.profiles.pros[0].TraceID)
	}

	// Без ключа → 401.
	resp = s.postPprof(t, body, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key status = %d, want 401", resp.StatusCode)
	}

	// Мусорное тело → 400.
	resp = s.postPprof(t, []byte("garbage"), "", s.key.PublicKey)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage status = %d, want 400", resp.StatusCode)
	}
}

// TestPprofDecompressionBomb — pprof присылают gzip'ом внутри тела без
// Content-Encoding, поэтому h.body его не разжимает. Маленький gzip (<maxBytes),
// раздувающийся за maxBytes*10 (=10 МБ при maxBytes 1 МБ), должен отклоняться
// c 413, а не разжиматься без предела (OOM) внутри ParsePprof.
func TestPprofDecompressionBomb(t *testing.T) {
	s := newStack(t)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(make([]byte, 11<<20)); err != nil { // 11 МБ нулей > 10 МБ лимита
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if buf.Len() >= 1<<20 {
		t.Fatalf("gzip payload %d bytes — не влезает под maxBytes, тест некорректен", buf.Len())
	}

	resp := s.postPprof(t, buf.Bytes(), "?type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("bomb status = %d, want 413", resp.StatusCode)
	}
	if s.profiles.count() != 0 {
		t.Fatalf("sink got %d profiles, want 0 (bomb must not be stored)", s.profiles.count())
	}
}

// TestPprofValidGzip — обычный клиент шлёт pprof одним слоем gzip (без
// Content-Encoding). После цикла распаковки ParseData получает protobuf и
// принимает профиль (202) — фикс от бомбы не должен ломать легитимный ввод.
func TestPprofValidGzip(t *testing.T) {
	s := newStack(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(pprofBody(t)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	resp := s.postPprof(t, gz.Bytes(), "?service=api&type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid gzipped pprof status = %d, want 202", resp.StatusCode)
	}
	if s.profiles.count() != 1 {
		t.Fatalf("sink got %d, want 1", s.profiles.count())
	}
}

// TestPprofNestedGzipBomb — двойной gzip: маленькое внешнее тело разжимается в
// внутренний gzip под лимитом, который pp.ParseData разжал бы БЕЗ предела.
// gunzipLimited должен размотать оба слоя под лимитом и отклонить бомбу (413),
// а не дать ParseData повторно разжать внутренний слой.
func TestPprofNestedGzipBomb(t *testing.T) {
	s := newStack(t)

	// Внутренний слой: 11 МБ нулей gzip'ом (> 10 МБ лимита распакованного слоя).
	var inner bytes.Buffer
	iz := gzip.NewWriter(&inner)
	if _, err := iz.Write(make([]byte, 11<<20)); err != nil {
		t.Fatalf("inner gzip: %v", err)
	}
	if err := iz.Close(); err != nil {
		t.Fatalf("inner close: %v", err)
	}
	// Внешний слой: gzip внутреннего gzip'а.
	var outer bytes.Buffer
	oz := gzip.NewWriter(&outer)
	if _, err := oz.Write(inner.Bytes()); err != nil {
		t.Fatalf("outer gzip: %v", err)
	}
	if err := oz.Close(); err != nil {
		t.Fatalf("outer close: %v", err)
	}
	if outer.Len() >= 1<<20 {
		t.Fatalf("outer payload %d bytes — не влезает под maxBytes, тест некорректен", outer.Len())
	}

	resp := s.postPprof(t, outer.Bytes(), "?type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("nested bomb status = %d, want 413", resp.StatusCode)
	}
	if s.profiles.count() != 0 {
		t.Fatalf("sink got %d profiles, want 0 (nested bomb must not be stored)", s.profiles.count())
	}
}
