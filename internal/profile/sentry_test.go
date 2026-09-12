package profile

import (
	"strconv"
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

func TestParseSentryCapsMetaFields(t *testing.T) {
	big := strings.Repeat("Ж", maxMetaField+300) // многобайтные руны — кап именно по рунам, не по байтам
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

func buildIndexedProfile(nFrames, nStacks, depth, fieldLen int) []byte {
	var b strings.Builder
	b.WriteString(`{"platform":"go","transaction":{"name":"t"},"profile":{"frames":[`)
	for i := 0; i < nFrames; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"function":"` + strings.Repeat("F", fieldLen) +
			`","filename":"` + strings.Repeat("f", fieldLen) + `","lineno":1}`)
	}
	b.WriteString(`],"stacks":[`)
	for s := 0; s < nStacks; s++ {
		if s > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for d := 0; d < depth; d++ {
			if d > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa((s*depth + d) % nFrames))
		}
		b.WriteByte(']')
	}
	b.WriteString(`],"samples":[`)
	for s := 0; s < nStacks; s++ {
		if s > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"stack_id":` + strconv.Itoa(s) + `}`)
	}
	b.WriteString(`]}}`)
	return []byte(b.String())
}

func TestParseSentryBoundsExpandedFrames(t *testing.T) {
	// один кадр, упомянутый maxFrames раз в 1000 стеках — форма атаки: в JSON
	// это индексы, в памяти гигабайты.
	raw := buildIndexedProfile(1, 1000, maxFrames, maxFrameField)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
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

func TestParseSentryKeepsRealisticProfile(t *testing.T) {
	const nStacks, depth = 2000, 40
	raw := buildIndexedProfile(2000, nStacks, depth, 60)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
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

func TestParseSentryCapsFrameTableOnce(t *testing.T) {
	raw := buildIndexedProfile(1, 1, 8, maxFrameField+100)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
	}
	if len(p.Samples) != 1 {
		t.Fatalf("сэмплов %d, want 1", len(p.Samples))
	}
	for _, f := range p.Samples[0].Stack {
		if len([]rune(f.Function)) != maxFrameField || len([]rune(f.File)) != maxFrameField {
			t.Fatalf("кадр не каппирован: function=%d file=%d, want %d",
				len([]rune(f.Function)), len([]rune(f.File)), maxFrameField)
		}
	}
}

func TestParseSentryTruncationIsDeterministic(t *testing.T) {
	raw := buildIndexedProfile(1, 1000, maxFrames, maxFrameField)

	first, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
	}
	if len(first.Samples) == 0 || len(first.Samples) == 1000 {
		t.Fatalf("бюджет должен был срезать часть стеков, получено %d", len(first.Samples))
	}

	for i := 0; i < 5; i++ {
		again, err := ParseSentry(raw, time.Now())
		if err != nil {
			t.Fatalf("ParseSentry #%d: %v", i, err)
		}
		if len(again.Samples) != len(first.Samples) {
			t.Fatalf("прогон #%d дал %d стеков вместо %d — усечение недетерминировано",
				i, len(again.Samples), len(first.Samples))
		}
		for j := range first.Samples {
			if again.Samples[j].Value != first.Samples[j].Value ||
				len(again.Samples[j].Stack) != len(first.Samples[j].Stack) {
				t.Fatalf("прогон #%d: стек %d отличается — усечение недетерминировано", i, j)
			}
		}
	}
}

func TestParseSentryKeepsHeaviestStacks(t *testing.T) {
	raw := []byte(`{"platform":"go","transaction":{"name":"t"},"profile":{` +
		`"frames":[{"function":"f0","filename":"a.go","lineno":1},` +
		`{"function":"f1","filename":"b.go","lineno":2},` +
		`{"function":"f2","filename":"c.go","lineno":3}],` +
		`"stacks":[[0],[1],[2]],` +
		`"samples":[{"stack_id":0},{"stack_id":1},{"stack_id":1},` +
		`{"stack_id":2},{"stack_id":2},{"stack_id":2}]}}`)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
	}
	if len(p.Samples) != 3 {
		t.Fatalf("сэмплов %d, want 3", len(p.Samples))
	}
	for i := 1; i < len(p.Samples); i++ {
		if p.Samples[i-1].Value < p.Samples[i].Value {
			t.Fatalf("стеки не отсортированы по убыванию веса: %d < %d",
				p.Samples[i-1].Value, p.Samples[i].Value)
		}
	}
	if p.Samples[0].Value != 3 {
		t.Fatalf("первым идёт стек с весом %d, want 3", p.Samples[0].Value)
	}
}

func TestParseSentryBudgetCountsEmptyFrames(t *testing.T) {
	raw := buildIndexedProfile(1, 1000, maxFrames, 0)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
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
