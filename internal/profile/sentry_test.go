package profile

import (
	"testing"
	"time"
)

func TestParseSentry(t *testing.T) {
	raw := []byte(`{
		"platform":"python","environment":"prod","transaction":{"name":"GET /x","trace_id":"trace-abc"},
		"profile":{
			"frames":[{"function":"main","filename":"m.py","lineno":1},
			          {"function":"handler","filename":"h.py","lineno":9},
			          {"function":"slow","filename":"s.py","lineno":20}],
			"stacks":[[2,1,0],[1,0]],
			"samples":[{"stack_id":0},{"stack_id":0},{"stack_id":1}]
		}
	}`)
	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Platform != "python" || p.Environment != "prod" || p.Transaction != "GET /x" || p.Type != "cpu" {
		t.Fatalf("meta = %+v", p)
	}
	if p.Service != "" {
		t.Fatalf("Service should be empty for Sentry profile, got %q", p.Service)
	}
	if p.TraceID != "trace-abc" {
		t.Fatalf("TraceID = %q, want trace-abc", p.TraceID)
	}
	// Два уникальных стека: [main,handler,slow] value 2, [main,handler] value 1.
	if len(p.Samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(p.Samples))
	}
	byLeaf := map[string]Sample{}
	for _, s := range p.Samples {
		byLeaf[s.Stack[len(s.Stack)-1].Function] = s
	}
	slow := byLeaf["slow"]
	if slow.Value != 2 || slow.Stack[0].Function != "main" || slow.Stack[2].Function != "slow" {
		t.Fatalf("slow stack (root->leaf) = %+v", slow)
	}
	if byLeaf["handler"].Value != 1 {
		t.Fatalf("handler value = %d", byLeaf["handler"].Value)
	}
}

func TestParseSentryBadJSON(t *testing.T) {
	if _, err := ParseSentry([]byte("{bad"), time.Now()); err == nil {
		t.Fatal("bad json must error")
	}
}
