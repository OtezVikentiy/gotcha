package templates

import (
	"context"
	"strings"
	"testing"
)

func TestMaintenanceKindIsFieldset(t *testing.T) {
	var sb strings.Builder
	if err := maintenanceFields(FormState{}, "uptime.maintenance.create.submit", "", "new-maintenance-window").Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "<fieldset") || !strings.Contains(out, `<legend class="field-label">`) {
		t.Errorf("группа radio «Тип окна» не обёрнута в fieldset/legend: %s", out)
	}
}
