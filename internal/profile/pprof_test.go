package profile

import (
	"bytes"
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
			// Location лист→корень: slow (лист), main (корень).
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
