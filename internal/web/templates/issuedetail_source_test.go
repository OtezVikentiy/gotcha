package templates

import (
	"context"
	"strings"
	"testing"
)

// frameSourceLines должен собрать pre+current+post со сквозной нумерацией и
// пометить строку ошибки Current — иначе подсветка/номера в стектрейсе врут.
func TestFrameSourceLines(t *testing.T) {
	f := Frame{
		Lineno:      10,
		ContextLine: "        throw new RuntimeException();",
		PreContext:  []string{"line8", "line9"},
		PostContext: []string{"line11", "line12"},
	}
	lines := frameSourceLines(f)
	if len(lines) != 5 {
		t.Fatalf("строк = %d, want 5", len(lines))
	}
	wantNo := []int{8, 9, 10, 11, 12} // pre=8,9; current=10; post=11,12
	for i, l := range lines {
		if l.No != wantNo[i] {
			t.Fatalf("line[%d].No = %d, want %d", i, l.No, wantNo[i])
		}
	}
	if !lines[2].Current {
		t.Fatal("строка ошибки (индекс 2) должна быть Current")
	}
	if lines[0].Current || lines[4].Current {
		t.Fatal("контекстные строки не должны быть Current")
	}
	if lines[2].Code != f.ContextLine {
		t.Fatalf("Code строки ошибки = %q", lines[2].Code)
	}
}

func TestFrameSourceLinesEmpty(t *testing.T) {
	if got := frameSourceLines(Frame{Function: "f"}); got != nil {
		t.Fatalf("без исходника ожидали nil, получили %v", got)
	}
	if frameHasSource(Frame{Function: "f"}) {
		t.Fatal("frameHasSource должен быть false без исходника")
	}
	if !frameHasSource(Frame{ContextLine: "x"}) {
		t.Fatal("frameHasSource должен быть true при наличии context_line")
	}
}

// prettyJSON форматирует валидный JSON с отступами; невалидный отдаёт как есть.
func TestPrettyJSON(t *testing.T) {
	got := prettyJSON(`{"os":{"name":"Linux"},"runtime":{"version":"8.4"}}`)
	if !strings.Contains(got, "\n") {
		t.Fatalf("ожидали многострочный JSON, получили: %q", got)
	}
	if !strings.Contains(got, "  \"os\"") {
		t.Fatalf("ожидали отступы, получили: %q", got)
	}
	if prettyJSON("not json") != "not json" {
		t.Fatal("невалидный JSON должен возвращаться как есть")
	}
}

// in-app фрейм с исходником: рендерит блок кода, подсветку строки ошибки и номер.
func TestFrameViewRendersSource(t *testing.T) {
	f := Frame{
		InApp:       true,
		Function:    "App\\Controller::boom",
		Filename:    "/src/Controller/Boom.php",
		Lineno:      10,
		ContextLine: "throw new RuntimeException();",
		PreContext:  []string{"public function boom() {"},
		PostContext: []string{"}"},
	}
	var sb strings.Builder
	if err := frameView(f, true).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"frame-source", "src-line-current", "throw new RuntimeException();", ">10<"} {
		if !strings.Contains(out, want) {
			t.Fatalf("вывод не содержит %q:\n%s", want, out)
		}
	}
}

// system-фрейм с исходником: свёрнут в <details>, но исходник внутри есть.
func TestFrameViewSystemRendersSource(t *testing.T) {
	f := Frame{
		Function:    "vendor\\thing",
		Filename:    "/vendor/thing.php",
		Lineno:      5,
		ContextLine: "->run();",
	}
	var sb strings.Builder
	if err := frameView(f, true).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "frame-system") || !strings.Contains(out, "frame-source") {
		t.Fatalf("system-фрейм должен иметь и <details>, и исходник:\n%s", out)
	}
}

// frameVars разбирает JSON локальных переменных в строки; пустые → false.
func TestFrameVars(t *testing.T) {
	f := Frame{Vars: `{"user_id":42,"email":"a@b.io"}`}
	if !frameHasVars(f) {
		t.Fatal("frameHasVars должен быть true")
	}
	got := map[string]string{}
	for _, r := range frameVars(f) {
		got[r.Key] = r.Val
	}
	if got["user_id"] != "42" || got["email"] != "a@b.io" {
		t.Fatalf("vars = %v", got)
	}
	if frameHasVars(Frame{Vars: "{}"}) || frameHasVars(Frame{Vars: "null"}) || frameHasVars(Frame{}) {
		t.Fatal("пустые vars должны давать false")
	}
}

func TestFrameViewRendersVars(t *testing.T) {
	f := Frame{InApp: true, Function: "f", Filename: "a.php", Lineno: 3, Vars: `{"x":"y"}`}
	var sb strings.Builder
	if err := frameView(f, false).Render(ruCtx(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "frame-vars") || !strings.Contains(out, "y") {
		t.Fatalf("переменные кадра не отрендерились: %s", out)
	}
}
