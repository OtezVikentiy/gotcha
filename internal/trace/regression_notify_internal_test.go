package trace

import (
	"context"
	"strings"
	"testing"
)

// TestRegressionNotifyDurationIsNotInflated: значение duration приходит в
// миллисекундах, и 640 мс должны выглядеть как 640 мс. Прежняя копия
// форматирования в этом файле (formatMetric) трактовала их как миллисекунды
// уже переведённого duration, но не отличала duration от веб-виталов и не
// проверяла итог против humanize.MetricValue — здесь фиксируем контракт:
// 640/400 не должны превращаться в "640.0s"/"400.0s".
//
// Тест лежит в этом (внутреннем, package trace) файле, а не в
// regression_notify_test.go, потому что regressionSubject не экспортирован, а
// regression_notify_test.go — блэкбокс (package trace_test).
func TestRegressionNotifyDurationIsNotInflated(t *testing.T) {
	ctx := context.Background()
	ev := RegressionEvent{
		Kind: "regression_open", Target: "GET /api/items", Metric: "duration",
		BaselineValue: 400, CurrentValue: 640, PctIncrease: 0.6,
	}
	subj := regressionSubject(ctx, ev)
	if strings.Contains(subj, "640.0s") || strings.Contains(subj, "400.0s") {
		t.Fatalf("тема письма = %q: миллисекунды показаны как секунды", subj)
	}
	if !strings.Contains(subj, "640ms") || !strings.Contains(subj, "400ms") {
		t.Errorf("тема письма = %q, хотим значения в мс", subj)
	}
}
