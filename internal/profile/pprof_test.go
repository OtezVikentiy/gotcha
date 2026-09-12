package profile

import (
	"bytes"
	"strings"
	"testing"
	"time"

	pp "github.com/google/pprof/profile"
)

func TestParsePprof(t *testing.T) {
	fnMain := &pp.Function{ID: 1, Name: "main", Filename: "m.go"}
	fnSlow := &pp.Function{ID: 2, Name: "slow", Filename: "s.go"}
	locMain := &pp.Location{ID: 1, Line: []pp.Line{{Function: fnMain, Line: 10}}}
	locSlow := &pp.Location{ID: 2, Line: []pp.Line{{Function: fnSlow, Line: 20}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fnMain, fnSlow},
		Location:   []*pp.Location{locMain, locSlow},
		Sample: []*pp.Sample{
			{Location: []*pp.Location{locSlow, locMain}, Value: []int64{7}},
		},
	}
	var buf bytes.Buffer
	if err := prof.Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ParsePprof(buf.Bytes(), "samples", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Type != "samples" {
		t.Fatalf("type = %q", got.Type)
	}
	if len(got.Samples) != 1 || got.Samples[0].Value != 7 {
		t.Fatalf("samples = %+v", got.Samples)
	}
	st := got.Samples[0].Stack
	if len(st) != 2 || st[0].Function != "main" || st[1].Function != "slow" {
		t.Fatalf("stack (root->leaf) = %+v", st)
	}
}

func TestParsePprofBad(t *testing.T) {
	if _, err := ParsePprof([]byte("not-a-pprof"), "", time.Now()); err == nil {
		t.Fatal("bad pprof must error")
	}
}

// buildPprofOneLocationManyLines разделяет один Location на nStacks сэмплов —
// форма реальной атаки: на проводе крохотный профиль, в памяти счётные капы перемножаются.
func buildPprofOneLocationManyLines(nLines, nStacks, fieldLen int) []byte {
	lines := make([]pp.Line, nLines)
	functions := make([]*pp.Function, nLines)
	for i := 0; i < nLines; i++ {
		fn := &pp.Function{
			ID:       uint64(i + 1),
			Name:     strings.Repeat("F", fieldLen),
			Filename: strings.Repeat("f", fieldLen),
		}
		functions[i] = fn
		lines[i] = pp.Line{Function: fn, Line: int64(i + 1)}
	}
	loc := &pp.Location{ID: 1, Line: lines}
	samples := make([]*pp.Sample, nStacks)
	for s := 0; s < nStacks; s++ {
		samples[s] = &pp.Sample{Location: []*pp.Location{loc}, Value: []int64{1}}
	}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   functions,
		Location:   []*pp.Location{loc},
		Sample:     samples,
	}
	var buf bytes.Buffer
	if err := prof.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func buildPprofProfile(nFrames, nStacks, depth, fieldLen int) []byte {
	functions := make([]*pp.Function, nFrames)
	locations := make([]*pp.Location, nFrames)
	for i := 0; i < nFrames; i++ {
		fn := &pp.Function{
			ID:       uint64(i + 1),
			Name:     strings.Repeat("F", fieldLen),
			Filename: strings.Repeat("f", fieldLen),
		}
		functions[i] = fn
		locations[i] = &pp.Location{ID: uint64(i + 1), Line: []pp.Line{{Function: fn, Line: int64(i + 1)}}}
	}
	samples := make([]*pp.Sample, nStacks)
	for s := 0; s < nStacks; s++ {
		locs := make([]*pp.Location, depth)
		for d := 0; d < depth; d++ {
			locs[d] = locations[(s*depth+d)%nFrames]
		}
		samples[s] = &pp.Sample{Location: locs, Value: []int64{1}}
	}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   functions,
		Location:   locations,
		Sample:     samples,
	}
	var buf bytes.Buffer
	if err := prof.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestParsePprofBoundsExpandedFrames(t *testing.T) {
	raw := buildPprofOneLocationManyLines(maxFrames, 200, maxFrameField)

	p, err := ParsePprof(raw, "samples", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}

	var bytesOut int
	for _, s := range p.Samples {
		for _, f := range s.Stack {
			bytesOut += len(f.Function) + len(f.File)
		}
	}
	// перерасход не больше одного кадра допустим: проверка бюджета стоит перед добавлением.
	limit := maxStackBytes + 2*maxFrameField
	if bytesOut > limit {
		t.Fatalf("развёрнуто %d байт кадров при бюджете %d — усиление не ограничено", bytesOut, limit)
	}
	if len(p.Samples) == 0 {
		t.Fatal("бюджет срезал профиль целиком, ожидалась частичная выдача")
	}
}

func TestParsePprofKeepsRealisticProfile(t *testing.T) {
	const nStacks, depth = 200, 20
	raw := buildPprofProfile(nStacks, nStacks, depth, 60)

	p, err := ParsePprof(raw, "samples", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	if len(p.Samples) != nStacks {
		t.Fatalf("получено %d стеков из %d — бюджет режет нормальный профиль", len(p.Samples), nStacks)
	}
	for _, s := range p.Samples {
		if len(s.Stack) != depth {
			t.Fatalf("глубина стека %d, want %d", len(s.Stack), depth)
		}
	}
}

func TestParsePprofBudgetCountsEmptyFrames(t *testing.T) {
	raw := buildPprofOneLocationManyLines(maxFrames, 2000, 0)

	p, err := ParsePprof(raw, "samples", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	frames := 0
	for _, s := range p.Samples {
		frames += len(s.Stack)
	}
	limit := maxStackBytes/frameOverheadBytes + maxFrames
	if frames > limit {
		t.Fatalf("развёрнуто %d кадров с пустыми именами при потолке %d — бюджет обходится", frames, limit)
	}
	if frames == 0 {
		t.Fatal("бюджет срезал профиль целиком")
	}
}

func TestParsePprofTruncatesMidStack(t *testing.T) {
	t.Run("countCapWithinOneLocation", func(t *testing.T) {
		const nLines = 2000 // заведомо больше maxFrames в ОДНОМ Location
		raw := buildPprofOneLocationManyLines(nLines, 1, 0)

		p, err := ParsePprof(raw, "samples", time.Now())
		if err != nil {
			t.Fatalf("ParsePprof: %v", err)
		}
		if len(p.Samples) != 1 {
			t.Fatalf("samples = %d, want 1", len(p.Samples))
		}
		if got := len(p.Samples[0].Stack); got != maxFrames {
			t.Fatalf("длина стека = %d, want %d — счётный кап внутри одного Location из %d Line не сработал", got, maxFrames, nLines)
		}
	})

	t.Run("budgetMidStack", func(t *testing.T) {
		// bytesPerFrame=164 не делит бюджет на целое число стеков по maxFrames —
		// исчерпание должно прийтись на середину одного из стеков, а не на его границу.
		const nStacks, fieldLen = 200, 50
		raw := buildPprofOneLocationManyLines(maxFrames, nStacks, fieldLen)

		p, err := ParsePprof(raw, "samples", time.Now())
		if err != nil {
			t.Fatalf("ParsePprof: %v", err)
		}
		if len(p.Samples) >= nStacks {
			t.Fatalf("получено %d стеков из %d — бюджет не исчерпался вовсе", len(p.Samples), nStacks)
		}
		partial := 0
		for _, s := range p.Samples {
			if n := len(s.Stack); n > 0 && n < maxFrames {
				partial++
			}
		}
		if partial == 0 {
			t.Fatal("ни один стек не обрезан посередине — бюджет останавливает сборку только на границе кадра или Location, не внутри неё")
		}
	})
}

func BenchmarkParsePprofExplosiveInput(b *testing.B) {
	raw := buildPprofOneLocationManyLines(1024, 100000, 0)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParsePprof(raw, "samples", time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
