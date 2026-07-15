package ingest_test

import (
	"bytes"
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
