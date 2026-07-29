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

// buildIndexedProfile собирает Sentry-профиль с nStacks стеками глубиной depth,
// ссылающимися на nFrames кадров с именами длиной fieldLen.
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

// TestParseSentryBoundsExpandedFrames фиксирует P0-усиление памяти.
//
// Формат Sentry индексный: stacks[] — индексы в frames[]. Счётных капов
// (maxFrames на стек × maxStacks на профиль) не хватало, они перемножались и
// разрешали 1024×100000 развёрнутых кадров по 2×maxFrameField байт каждый.
// Измерено на старом коде: 6.1 КиБ gzip-тела (индексы «0,0,0,…» жмутся в 330
// раз) → 8.6 ГиБ аллокаций и 12.6 с CPU на горутине запроса, с публичного
// DSN-ключа. Бюджет maxStackBytes ограничивает работу независимо от того, как
// разложены стеки.
func TestParseSentryBoundsExpandedFrames(t *testing.T) {
	// Один кадр с максимально длинными именами, упомянутый 1024 раза в каждом
	// из 1000 стеков — форма атаки: в JSON это индексы, в памяти гигабайты.
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
	// Бюджет — потолок; допускаем перерасход не больше одного кадра, потому что
	// проверка стоит перед добавлением.
	limit := maxStackBytes + 2*maxFrameField
	if bytesOut > limit {
		t.Fatalf("развёрнуто %d байт кадров при бюджете %d — усиление не ограничено", bytesOut, limit)
	}
	if len(p.Samples) == 0 {
		t.Fatal("бюджет срезал профиль целиком, ожидалась частичная выдача")
	}
}

// TestParseSentryKeepsRealisticProfile — обратная сторона бюджета: он не должен
// трогать профиль правдоподобной формы. 2000 стеков глубиной 40 с именами
// обычной длины — это 80 000 кадров, и они обязаны дойти целиком.
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

// TestParseSentryCapsFrameTableOnce — кадр каппится один раз в таблице, а не на
// каждом упоминании: именно per-упоминание capRunes давал 6.4 ГиБ из 8.6.
// Проверяем результат кападжа при многократной ссылке на один и тот же кадр.
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

// TestParseSentryTruncationIsDeterministic — при срабатывании бюджета два
// одинаковых байт-в-байт профиля обязаны дать одинаковый результат.
//
// Обход map случаен, а с введением байтового бюджета усечение стало штатным
// путём: без сортировки один и тот же профиль сохранял бы разные подмножества
// стеков, и флеймграф менялся бы от загрузки к загрузке.
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

// TestParseSentryKeepsHeaviestStacks — бюджет тратится на стеки с наибольшим
// весом, а не на случайные: срезанный профиль должен оставаться осмысленным.
func TestParseSentryKeepsHeaviestStacks(t *testing.T) {
	// Три стека, у первого вес 1, у второго 2, у третьего 3 (число сэмплов).
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
	// Порядок — по убыванию веса.
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

// TestParseSentryBudgetCountsEmptyFrames — бюджет обязан ограничивать и кадры с
// ПУСТЫМИ именами.
//
// Раньше он списывался как len(function)+len(filename), поэтому пустые имена
// стоили ноль и защита откатывалась к счётным капам: maxStacks × maxFrames =
// 102 млн развёрнутых кадров. Измерено на таком входе: 47 КБ gzip → ~4 млн
// кадров и ~240 МиБ аллокаций на горутине одного запроса, с публичного
// DSN-ключа.
func TestParseSentryBudgetCountsEmptyFrames(t *testing.T) {
	// Один кадр с пустыми именами, упомянутый maxFrames раз в 1000 стеках.
	raw := buildIndexedProfile(1, 1000, maxFrames, 0)

	p, err := ParseSentry(raw, time.Now())
	if err != nil {
		t.Fatalf("ParseSentry: %v", err)
	}
	frames := 0
	for _, s := range p.Samples {
		frames += len(s.Stack)
	}
	// Бюджет / стоимость кадра — верхняя граница числа развёрнутых кадров.
	limit := maxStackBytes/frameOverheadBytes + maxFrames
	if frames > limit {
		t.Fatalf("развёрнуто %d кадров с пустыми именами при потолке %d — бюджет обходится", frames, limit)
	}
	if frames == 0 {
		t.Fatal("бюджет срезал профиль целиком")
	}
}
