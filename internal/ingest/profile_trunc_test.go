package ingest_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	pp "github.com/google/pprof/profile"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

// Несжатый протобуф с n минимальными Sample на общем Location — так ParsePprof
// видит вход после gunzipLimited.
func pprofBodyManySamples(t *testing.T, n int) []byte {
	t.Helper()
	fn := &pp.Function{ID: 1, Name: "f"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
	}
	prof.Sample = make([]*pp.Sample, n)
	for i := 0; i < n; i++ {
		prof.Sample[i] = &pp.Sample{Location: []*pp.Location{loc}, Value: []int64{1}}
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	return buf.Bytes()
}

// Один сэмпл, но в его Location больше кадров, чем maxFrames — усечение по
// кадрам, не по числу сэмплов.
func pprofBodyManyFramesOneStack(t *testing.T, nLines int) []byte {
	t.Helper()
	lines := make([]pp.Line, nLines)
	functions := make([]*pp.Function, nLines)
	for i := 0; i < nLines; i++ {
		fn := &pp.Function{ID: uint64(i + 1), Name: fmt.Sprintf("f%d", i)}
		functions[i] = fn
		lines[i] = pp.Line{Function: fn, Line: int64(i + 1)}
	}
	loc := &pp.Location{ID: 1, Line: lines}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   functions,
		Location:   []*pp.Location{loc},
		Sample:     []*pp.Sample{{Location: []*pp.Location{loc}, Value: []int64{5}}},
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	return buf.Bytes()
}

func TestPprofEndpointRejectsOversizedSampleCount(t *testing.T) {
	s := newStack(t)
	// Заведомо выше потолка profile-пакета (100000 на момент задачи) — само число
	// не привязываем к приватной константе другого пакета, важно, что оно её превышает.
	body := pprofBodyManySamples(t, 150000)

	resp := s.postPprof(t, body, "?type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if s.profiles.count() != 0 {
		t.Fatalf("sink got %d profiles, want 0 (oversized must not be decoded)", s.profiles.count())
	}
	if got := s.h.RejectedBy(ingest.RejectTooLarge, ingest.SignalProfile); got == 0 {
		t.Fatal("gotcha_ingest_rejected_total{reason=too_large,signal=profile} не выросла")
	}
}

func TestPprofEndpointCountsTruncation(t *testing.T) {
	s := newStack(t)
	before := s.h.ProfileTruncatedBy(ingest.ParserPprof)
	body := pprofBodyManyFramesOneStack(t, 2000) // maxFrames = 1024

	resp := s.postPprof(t, body, "?type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if s.profiles.count() != 1 {
		t.Fatalf("sink got %d, want 1", s.profiles.count())
	}
	if !s.profiles.pros[0].Truncated {
		t.Fatal("Profile.Truncated не взведён при обрезанном стеке")
	}
	if got := s.h.ProfileTruncatedBy(ingest.ParserPprof) - before; got != 1 {
		t.Fatalf("gotcha_ingest_profile_truncated_total{parser=pprof} += %d, want 1", got)
	}
	if got := s.h.ProfileTruncatedBy(ingest.ParserSentry); got != 0 {
		t.Fatalf("parser=sentry не должен расти от pprof-пути, got %d", got)
	}
}

func sentryProfileEnvelopeItem(nLines int) string {
	frames := make([]string, nLines)
	for i := range frames {
		frames[i] = fmt.Sprintf(`{"function":"f%d","filename":"f.go","lineno":%d}`, i, i+1)
	}
	idxs := make([]string, nLines)
	for i := range idxs {
		idxs[i] = fmt.Sprintf("%d", i)
	}
	return `{"type":"profile"}` + "\n" +
		`{"platform":"go","transaction":{"name":"t"},"profile":{"frames":[` +
		strings.Join(frames, ",") + `],"stacks":[[` + strings.Join(idxs, ",") +
		`]],"samples":[{"stack_id":0}]}}` + "\n"
}

func TestSentryProfileEnvelopeCountsTruncation(t *testing.T) {
	s := newStack(t)
	before := s.h.ProfileTruncatedBy(ingest.ParserSentry)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)
	body := "{}\n" + sentryProfileEnvelopeItem(2000) // maxFrames = 1024

	resp := s.post(t, path, body, false, s.key.PublicKey)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if s.profiles.count() != 1 {
		t.Fatalf("sink got %d, want 1", s.profiles.count())
	}
	if !s.profiles.pros[0].Truncated {
		t.Fatal("Profile.Truncated не взведён при обрезанном стеке")
	}
	if got := s.h.ProfileTruncatedBy(ingest.ParserSentry) - before; got != 1 {
		t.Fatalf("gotcha_ingest_profile_truncated_total{parser=sentry} += %d, want 1", got)
	}
	if got := s.h.ProfileTruncatedBy(ingest.ParserPprof); got != 0 {
		t.Fatalf("parser=pprof не должен расти от sentry-пути, got %d", got)
	}
}

// Отказ бюджета разбора обязан быть отличим от просевшего буфера записи: свой
// reason в gotcha_ingest_rejected_total, RejectOverloaded не растёт вовсе.
func TestPprofEndpointBudgetExhaustionIsNotOverloaded(t *testing.T) {
	s := newStack(t)
	s.h.SetProfileDecodeBudgetBytes(1) // любое непустое тело весит больше 1

	beforeBudget := s.h.RejectedBy(ingest.RejectProfileDecodeBudget, ingest.SignalProfile)
	beforeOverloaded := s.h.RejectedBy(ingest.RejectOverloaded, ingest.SignalProfile)
	body := pprofBodyManySamples(t, 1)

	resp := s.postPprof(t, body, "?type=samples", s.key.PublicKey)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if s.profiles.count() != 0 {
		t.Fatalf("sink got %d profiles, want 0", s.profiles.count())
	}
	if got := s.h.RejectedBy(ingest.RejectProfileDecodeBudget, ingest.SignalProfile) - beforeBudget; got != 1 {
		t.Fatalf("RejectedBy(profile_decode_budget, profile) += %d, want 1", got)
	}
	if got := s.h.RejectedBy(ingest.RejectOverloaded, ingest.SignalProfile); got != beforeOverloaded {
		t.Fatalf("RejectedBy(overloaded, profile) изменился (%d -> %d) — отказ бюджета разбора не должен маскироваться под перегрузку буфера",
			beforeOverloaded, got)
	}
}
