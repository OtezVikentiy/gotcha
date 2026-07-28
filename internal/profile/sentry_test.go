package profile

import (
	"strings"
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

// TestParseSentryCapsMetaFields: недоверенные строковые поля каппятся до
// maxMetaField рун перед записью (иначе раздувают колонки profiles).
func TestParseSentryCapsMetaFields(t *testing.T) {
	big := strings.Repeat("Ж", maxMetaField+300) // многобайтные руны — проверяем кап именно по рунам
	raw := []byte(`{
		"platform":"` + big + `","environment":"` + big + `",
		"transaction":{"name":"` + big + `","trace_id":"` + big + `"},
		"profile":{"frames":[],"stacks":[],"samples":[]}
	}`)
	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, got := range map[string]string{
		"Platform":    p.Platform,
		"Environment": p.Environment,
		"Transaction": p.Transaction,
		"TraceID":     p.TraceID,
	} {
		if n := len([]rune(got)); n != maxMetaField {
			t.Fatalf("%s len = %d runes, want %d", name, n, maxMetaField)
		}
	}
}

// TestFrameFieldsCapped фиксирует P0 амплификации: формат Sentry индексный, поэтому
// одно огромное имя функции, упомянутое во всех кадрах стека, раздувалось в
// Writer.Add при склейке ключа (393 КБ тела → ~409 МБ аллокаций). Кап обязан
// стоять на РАЗБОРЕ, до амплификации.
func TestFrameFieldsCapped(t *testing.T) {
	huge := strings.Repeat("A", 400_000)
	body := `{"profile":{"frames":[{"function":"` + huge + `","filename":"` + huge + `"}],` +
		`"stacks":[[0,0,0,0]],"samples":[{"stack_id":0,"elapsed_since_start_ns":1,"thread_id":"1"}]},` +
		`"transaction":{"name":"t"},"platform":"go","environment":"prod"}`

	p, err := ParseSentry([]byte(body), time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
	}
	for _, s := range p.Samples {
		for _, f := range s.Stack {
			if len([]rune(f.Function)) > maxFrameField {
				t.Fatalf("Function не обрезан: %d рун (кап %d)", len([]rune(f.Function)), maxFrameField)
			}
			if len([]rune(f.File)) > maxFrameField {
				t.Fatalf("File не обрезан: %d рун (кап %d)", len([]rune(f.File)), maxFrameField)
			}
		}
	}
}
