package guards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfMetricsDocumented — №126: каждая метрика, реально зарегистрированная
// в дереве, обязана упоминаться в self-monitoring.md ОБОИХ языков.
//
// Инвентарь берётся из collectSelfMetrics (selfmetrics_names_test.go) — того
// же обхода, что кормит TestSelfMetricNamesPinned/TestSelfMetricQueueNamingCanon,
// а не из отдельного, второго AST-скана того же дерева. До этой правки здесь
// жил собственный, независимый обход с той же логикой распознавания
// call-site'а, но БЕЗ фикса на алиасированный/dot-импорт (E3, заморозка
// контракта self-метрик): регистрация через `import sm ".../selfmetrics"`
// была бы для НЕГО невидима точно так же, как когда-то была невидима для
// collectSelfMetrics, только тихо — этот тест не проверяет полноту (что
// найдено, то и сверяется с доками), поэтому пропавшая по алиасу метрика не
// уронила бы вообще ничего. Нелитеральные имя и тип метрики отдельно уже не
// проверяются — это делает сам collectSelfMetrics.
func TestSelfMetricsDocumented(t *testing.T) {
	tree := Load(t)
	scan := collectSelfMetrics(t, tree)
	if scan.callSites == 0 {
		t.Fatalf("blind guard: found 0 selfmetrics.Add/AddInt call-sites — the scan is looking at the wrong tree")
	}
	if len(scan.types) < 10 {
		t.Fatalf("collected only %d metrics — the scanner is broken", len(scan.types))
	}
	for _, lang := range []string{"en", "ru"} {
		doc, err := os.ReadFile(filepath.Join(tree.Root, "internal", "docs", lang, "self-monitoring.md"))
		if err != nil {
			t.Fatal(err)
		}
		for name := range scan.types {
			if !strings.Contains(string(doc), name) {
				t.Errorf("%s is registered (%s) but missing from %s/self-monitoring.md",
					name, strings.Join(scan.files[name], ", "), lang)
			}
		}
	}
}
