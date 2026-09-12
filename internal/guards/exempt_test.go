package guards

import (
	"strings"
	"testing"
)

func TestFuncContexts(t *testing.T) {
	body := strings.Join([]string{
		`package demo`,
		``,
		`func plain(a int) int {`,
		`	return a + 1`,
		`}`,
		``,
		`func (g geom) xFor(i int) float64 {`,
		`	return float64(i)`,
		`}`,
		`templ widget(name string) {`,
		`	<div>{ name }</div>`,
		`}`,
	}, "\n")

	ctx := funcContexts(body)
	lines := strings.Split(body, "\n")
	if len(ctx) != len(lines) {
		t.Fatalf("funcContexts вернул %d строк, want %d (по числу строк body)", len(ctx), len(lines))
	}

	cases := []struct {
		line int // 1-индексная, для читаемости
		want string
	}{
		{1, ""},
		{2, ""},
		{3, "plain"},
		{4, "plain"},
		{6, "plain"},
		{7, "geom.xFor"},
		{8, "geom.xFor"},
		{10, "widget"},
		{11, "widget"},
	}
	for _, c := range cases {
		if got := ctx[c.line-1]; got != c.want {
			t.Errorf("funcContexts: строка %d = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestFuncContextsRecognizesGenericFunctions(t *testing.T) {
	body := strings.Join([]string{
		`package demo`,
		``,
		`func before() {`,
		`	return`,
		`}`,
		``,
		`func Foo[T any](items []T) []T {`,
		`	return items`,
		`}`,
		``,
		`func Bar[K comparable, V any](m map[K]V) int {`,
		`	return len(m)`,
		`}`,
		``,
		`func fillSeries[T any](src []T, from, to time.Time, step time.Duration,`,
		`	at func(T) time.Time, gap func(time.Time) T) []T {`,
		`	return src`,
		`}`,
	}, "\n")

	ctx := funcContexts(body)
	cases := []struct {
		line int
		want string
	}{
		{3, "before"},
		{7, "Foo"},
		{8, "Foo"},
		{11, "Bar"},
		{12, "Bar"},
		{15, "fillSeries"},
		{16, "fillSeries"},
		{17, "fillSeries"},
	}
	for _, c := range cases {
		if got := ctx[c.line-1]; got != c.want {
			t.Errorf("funcContexts: строка %d = %q, want %q", c.line, got, c.want)
		}
	}
}

// Проверяет строку ВНУТРИ тела функции, не саму декларацию — до правки
// декларация распозналась бы и так, ложноположительно.
func TestFuncContextsRecognizesGenericFunctionsOnRealTree(t *testing.T) {
	tree := Load(t)
	byPath := map[string]string{}
	for _, f := range tree.GoFiles {
		byPath[f.Path] = f.Body
	}

	cases := []struct {
		path     string
		wantFunc string
	}{
		{"internal/chbatch/isolate.go", "IsolatePoison"},
		{"internal/host/resolve.go", "levelCandidates"},
		{"internal/host/resolve.go", "resolveKind"},
		{"internal/ingest/otlp.go", "joinAttrParts"},
		{"internal/web/gapfill.go", "fillSeries"},
	}
	for _, c := range cases {
		body, ok := byPath[c.path]
		if !ok {
			t.Fatalf("%s не найден в дереве — проба сломана или файл переехал", c.path)
		}
		ctx := funcContexts(body)
		lines := strings.Split(body, "\n")
		found := false
		for i, line := range lines {
			// Пропускаем саму декларацию, проверяем строку после — важно тело, не совпадение объявления.
			if strings.Contains(line, "func "+c.wantFunc+"[") && i+1 < len(ctx) {
				if got := ctx[i+1]; got != c.wantFunc {
					t.Errorf("%s: строка внутри тела %s (%q) = %q, want %q", c.path, c.wantFunc, strings.TrimSpace(lines[i+1]), got, c.wantFunc)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: объявление func %s[...]( не найдено в текущем файле — проба сломана или функция переименована", c.path, c.wantFunc)
		}
	}
}

func TestContentAnchor(t *testing.T) {
	got := ContentAnchor("internal/web/svg.go", "chartBars", `text := points[idx].T.UTC().Format("02.01")`)
	want := `internal/web/svg.go in chartBars: text := points[idx].T.UTC().Format("02.01")`
	if got != want {
		t.Errorf("ContentAnchor(с функцией) = %q, want %q", got, want)
	}

	gotTop := ContentAnchor("internal/web/foo.go", "", `var x = 1`)
	if !strings.Contains(gotTop, "top level") {
		t.Errorf("ContentAnchor(без функции) = %q, ожидался явный маркер верхнего уровня файла", gotTop)
	}
}

func TestRecordAnchorRejectsAmbiguousDifferentLines(t *testing.T) {
	ft := &fakeT{}
	seenLines := map[string]int{}

	recordAnchor(ft, "проба", seenLines, "demo.go in f: x.Format(layout)", 10)
	if ft.failed {
		t.Fatalf("первая запись якоря не должна проваливать тест, а уже провалила: %v", ft.msgs)
	}

	recordAnchor(ft, "проба", seenLines, "demo.go in f: x.Format(layout)", 25)
	ft.requireFailure(t, "неоднозначен")
}

func TestRecordAnchorAllowsSameAnchorSameLineTwice(t *testing.T) {
	ft := &fakeT{}
	seenLines := map[string]int{}

	recordAnchor(ft, "проба", seenLines, "demo.go in f: a + b", 42)
	recordAnchor(ft, "проба", seenLines, "demo.go in f: a + b", 42)

	if ft.failed {
		t.Fatalf("два совпадения НА ОДНОЙ строке не обязаны проваливать тест, а провалили: %v", ft.msgs)
	}
}
