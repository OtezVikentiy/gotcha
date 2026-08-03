package templates

import (
	"context"
	"strings"
	"testing"
)

// TestMaintenanceKindIsFieldset: группа radio «Тип окна» обязана быть
// fieldset/legend — без группы скринридер читает «Разовое» и «Еженедельное»
// как одиночные radio без общего вопроса (№79, WCAG 1.3.1).
func TestMaintenanceKindIsFieldset(t *testing.T) {
	var sb strings.Builder
	if err := maintenanceFields(FormState{}, "uptime.maintenance.create.submit", "").Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "<fieldset") || !strings.Contains(out, `<legend class="field-label">`) {
		t.Errorf("группа radio «Тип окна» не обёрнута в fieldset/legend: %s", out)
	}
}
