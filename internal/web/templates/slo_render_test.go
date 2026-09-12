package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// без ClickHouse на стенде провайдеры nil — HasData=true через хендлер не получить,
// поэтому VM здесь собран и отрендерен напрямую.
func TestSLOsScreenRendersDataRows(t *testing.T) {
	ctx := context.Background()
	rows := []SLORow{
		{ID: 1, Name: "checkout availability", Kind: "availability", TargetPct: 99, AttainmentPct: 99.5, BudgetRemainingPct: 50, HasData: true, Status: "healthy"},
		{ID: 2, Name: "search latency", Kind: "latency", TargetPct: 99, AttainmentPct: 98.5, BudgetRemainingPct: 12, HasData: true, Status: "burning"},
		{ID: 3, Name: "api uptime", Kind: "uptime", TargetPct: 99.9, AttainmentPct: 99.0, BudgetRemainingPct: -30, HasData: true, Status: "exhausted"},
		{ID: 4, Name: "no-data slo", Kind: "availability", TargetPct: 99, HasData: false},
	}
	monitors := []SLOMonitorOption{{ID: 7, Name: "api monitor"}}
	var sb strings.Builder
	if err := SLOsScreen(1, rows, monitors, FormState{}, "", "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"checkout availability", "search latency", "api uptime", "no-data slo", "api monitor"} {
		if !strings.Contains(out, want) {
			t.Errorf("SLOsScreen не содержит %q", want)
		}
	}
	if !strings.Contains(out, i18n.T(ctx, "slo.list.no_data_hint")) {
		t.Errorf("SLOsScreen не содержит подсказку slo.list.no_data_hint для строки без данных")
	}
	if !strings.Contains(out, i18n.T(ctx, "slo.form.burn_hint")) {
		t.Errorf("SLOsScreen не содержит подсказку slo.form.burn_hint в форме")
	}
}

func TestSLODetailScreenRendersFullState(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	resolved := started.Add(2 * time.Hour)
	rem := 40.0
	vm := SLODetailVM{
		ProjectID: 1, ID: 5, Name: "checkout availability", Kind: "availability",
		TargetPct: 99, WindowDays: 30, BurnThreshold: 14.4,
		HasData: true, AttainmentPct: 98.7, BudgetRemainingPct: -30, Status: "exhausted",
		HasBurn: true, BurnShort: 22.0, BurnLong: 18.0,
		HasOpenIncident: true,
		OpenIncident:    SLOIncidentRow{Open: true, StartedAt: started, BurnRate: 22.0, HasBudget: true, BudgetRemainingPct: rem},
		Chart:           templ.NopComponent,
		Incidents: []SLOIncidentRow{
			{Open: true, StartedAt: started, BurnRate: 22.0, HasBudget: true, BudgetRemainingPct: rem},
			{Open: false, StartedAt: started.Add(-48 * time.Hour), ResolvedAt: &resolved, BurnRate: 16.0, HasBudget: false},
		},
	}
	var sb strings.Builder
	if err := SLODetailScreen(vm, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"checkout availability", "×22.0", "×18.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("SLODetailScreen не содержит %q", want)
		}
	}
	// @relativeTime сразу после текста в templ уходит в разметку сырым литералом, не рендерясь.
	if strings.Contains(out, "@relativeTime") {
		t.Errorf("сырой templ-литерал @relativeTime в разметке: %s", out)
	}
	if !strings.Contains(out, "<time") {
		t.Errorf("открытый инцидент не отрендерил относительное время (<time>): %s", out)
	}
	if !strings.Contains(out, sloStatusLabel(ctx, "exhausted")) {
		t.Errorf("SLODetailScreen не содержит текстовый лейбл статуса %q", sloStatusLabel(ctx, "exhausted"))
	}
	if !strings.Contains(out, i18n.T(ctx, "slo.detail.burn_hint")) {
		t.Errorf("SLODetailScreen не содержит подсказку slo.detail.burn_hint")
	}

	var sb2 strings.Builder
	empty := SLODetailVM{ProjectID: 1, ID: 6, Name: "empty slo", Kind: "uptime", TargetPct: 99.9, WindowDays: 30, HasData: false, Chart: templ.NopComponent}
	if err := SLODetailScreen(empty, "u@example.com").Render(ctx, &sb2); err != nil {
		t.Fatalf("Render empty: %v", err)
	}
	if !strings.Contains(sb2.String(), "empty slo") {
		t.Errorf("пустой SLODetailScreen не содержит имя")
	}
}
