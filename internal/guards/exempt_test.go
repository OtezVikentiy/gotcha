package guards

import (
	"strings"
	"testing"
)

// TestFuncContexts проверяет, что funcContexts правильно относит каждую
// строку к ближайшей ПРЕДШЕСТВУЮЩЕЙ названной функции/templ-блоку — это
// единственный источник "различающего контекста", на который опирается
// ContentAnchor при девяти похожих местах в internal/web/svg.go (см. её
// докблок).
func TestFuncContexts(t *testing.T) {
	body := strings.Join([]string{
		`package demo`,                        // 1: до первого объявления — ""
		``,                                    // 2
		`func plain(a int) int {`,             // 3: plain
		`	return a + 1`,                       // 4: plain
		`}`,                                   // 5: plain (тело ещё внутри функции)
		``,                                    // 6: plain — funcContexts не видит "}",
		`func (g geom) xFor(i int) float64 {`, // 7: geom.xFor
		`	return float64(i)`,                  // 8: geom.xFor
		`}`,                                   // 9: geom.xFor
		`templ widget(name string) {`,         // 10: widget
		`	<div>{ name }</div>`,                // 11: widget
		`}`,                                   // 12: widget
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

// TestFuncContextsRecognizesGenericFunctions — доработка по ревью задачи
// W3-J: funcDeclRe изначально не понимал список типовых параметров
// дженерика (func Foo[T any](...)) — "[T any]" рвал `(\w+)\s*\(` сразу
// после имени, и всё внутри такой функции молча приписывалось предыдущей
// НАЗВАННОЙ функции выше по файлу — тот же класс тихой порчи контекста, что
// и у самих номеров строк. Ревьюер нашёл пять реальных пропущенных
// объявлений отдельным скриптом на go/ast (см. докблок funcDeclRe); эта
// проба закрывает синтетическую сторону, следующая ниже — живую.
//
// Три формы: один типовой параметр в однострочной сигнатуре (Foo), два
// типовых параметра (Bar — "[K comparable, V any]" не должен обрываться на
// первой запятой), и многострочная сигнатура (fillSeries — ровно форма
// internal/web/gapfill.go, где список параметров переносится на вторую
// строку уже ПОСЛЕ распознанного объявления).
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

// TestFuncContextsRecognizesGenericFunctionsOnRealTree — та же проба на
// живом дереве, а не на синтетике: все пять функций, которые ревью нашло
// пропущенными (internal/chbatch/isolate.go:27, internal/host/resolve.go:83
// и 102, internal/ingest/otlp.go:1436, internal/web/gapfill.go:16), обязаны
// правильно атрибутировать строку ВНУТРИ своего тела (не саму строку
// объявления — до правки она распознавалась бы тоже, ложноположительно, по
// случайному совпадению регэкспа "(\w+)\s*\(" на предыдущей функции).
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
			// Строка вида "func <wantFunc>[...](" — сама декларация,
			// намеренно пропускаем её и проверяем строку ПОСЛЕ: важно, что
			// ТЕЛО функции атрибутировано верно, а не только то, что
			// объявление где-то совпало.
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

// TestContentAnchor закрепляет форму ключа и явно проверяет случай пустого
// funcName (строка выше первого объявления в файле) — см. докблок
// ContentAnchor про то, что схема гарантирует.
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

// TestRecordAnchorRejectsAmbiguousDifferentLines — проба на неоднозначность,
// требуемая брифом W3-J: якорь, совпавший с двумя РАЗНЫМИ строками, обязан
// провалить тест с внятным сообщением, а не молча оставить первую находку.
// Без этого механизма два похожих места в одном файле, отличить которые
// текстово не удалось, тихо схлопнулись бы в одно исключение — и одна из
// находок осталась бы непроверенной сторожем навсегда.
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

// TestRecordAnchorAllowsSameAnchorSameLineTwice — обратная сторона той же
// пробы: ДВА совпадения на ОДНОЙ физической строке (в internal/web/svg.go
// есть строка с двумя вызовами .Format("02.01") подряд) обязаны остаться
// одним местом находки, а не провалить тест как мнимую неоднозначность —
// иначе сама схема оказалась бы строже старой exemptLoc, которая этот случай
// прямо разрешала (см. докблок recordAnchor).
func TestRecordAnchorAllowsSameAnchorSameLineTwice(t *testing.T) {
	ft := &fakeT{}
	seenLines := map[string]int{}

	recordAnchor(ft, "проба", seenLines, "demo.go in f: a + b", 42)
	recordAnchor(ft, "проба", seenLines, "demo.go in f: a + b", 42)

	if ft.failed {
		t.Fatalf("два совпадения НА ОДНОЙ строке не обязаны проваливать тест, а провалили: %v", ft.msgs)
	}
}
