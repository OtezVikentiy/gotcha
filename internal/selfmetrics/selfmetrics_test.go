package selfmetrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatherFormat(t *testing.T) {
	var r Registry
	r.AddInt(Gauge, "gotcha_buffer_rows", "Rows waiting to be written.",
		map[string]string{"writer": "events"}, func() int64 { return 42 })
	r.AddInt(Counter, "gotcha_dropped_total", "Rows dropped.",
		map[string]string{"writer": "events"}, func() int64 { return 7 })
	// Вторая серия того же имени — обязана попасть под ОДИН блок HELP/TYPE.
	r.AddInt(Counter, "gotcha_dropped_total", "Rows dropped.",
		map[string]string{"writer": "spans"}, func() int64 { return 0 })

	out := r.Gather()
	if strings.Count(out, "# HELP gotcha_dropped_total") != 1 {
		t.Errorf("HELP для одного имени должен быть один раз:\n%s", out)
	}
	if strings.Count(out, "# TYPE gotcha_dropped_total") != 1 {
		t.Errorf("TYPE для одного имени должен быть один раз:\n%s", out)
	}
	for _, want := range []string{
		`gotcha_buffer_rows{writer="events"} 42`,
		`gotcha_dropped_total{writer="events"} 7`,
		`gotcha_dropped_total{writer="spans"} 0`,
		"# TYPE gotcha_buffer_rows gauge",
		"# TYPE gotcha_dropped_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет строки %q в выводе:\n%s", want, out)
		}
	}
}

// Значения читаются на КАЖДЫЙ скрап, а не запоминаются при регистрации.
func TestGatherReadsLive(t *testing.T) {
	var r Registry
	n := int64(1)
	r.AddInt(Gauge, "g", "help", nil, func() int64 { return n })
	if !strings.Contains(r.Gather(), "g 1") {
		t.Fatal("первое чтение не 1")
	}
	n = 99
	if !strings.Contains(r.Gather(), "g 99") {
		t.Fatal("значение не перечитано на втором скрапе")
	}
}

func TestEscaping(t *testing.T) {
	var r Registry
	r.AddInt(Gauge, "esc", "line\nbreak and \\ slash", map[string]string{"v": `q"uote`}, func() int64 { return 1 })
	out := r.Gather()
	if !strings.Contains(out, `# HELP esc line\nbreak and \\ slash`) {
		t.Errorf("HELP не экранирован:\n%s", out)
	}
	if !strings.Contains(out, `esc{v="q\"uote"} 1`) {
		t.Errorf("значение метки не экранировано:\n%s", out)
	}
}

// Порядок вывода стабилен: одинаковый скрап — одинаковый текст.
func TestGatherStable(t *testing.T) {
	var r Registry
	r.AddInt(Gauge, "b", "b", map[string]string{"z": "1", "a": "2"}, func() int64 { return 1 })
	r.AddInt(Gauge, "a", "a", nil, func() int64 { return 1 })
	first := r.Gather()
	for i := 0; i < 5; i++ {
		if r.Gather() != first {
			t.Fatal("вывод нестабилен между скрапами")
		}
	}
	// Сортировка по имени: блок метрики "a" должен идти раньше блока "b".
	if strings.Index(first, "# HELP a ") > strings.Index(first, "# HELP b ") {
		t.Errorf("метрики не отсортированы по имени:\n%s", first)
	}
	if !strings.Contains(first, `b{a="2",z="1"}`) {
		t.Errorf("метки не отсортированы:\n%s", first)
	}
}

func TestHandlerContentType(t *testing.T) {
	var r Registry
	r.AddInt(Gauge, "x", "x", nil, func() int64 { return 1 })
	rec := httptest.NewRecorder()
	r.Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "x 1") {
		t.Errorf("тело: %s", rec.Body.String())
	}
}
