package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// TestSLOsScreenRendersDataRows покрывает ветку HasData=true списка SLO (индикатор
// достижения, бейдж статуса, проценты) — на веб-стенде без ClickHouse провайдеры
// nil, поэтому эти строки в web-тесте не рендерятся; здесь рендерим VM напрямую.
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
}

// TestSLODetailScreenRendersFullState покрывает ветки экрана деталей: HasData
// (карточки+график), HasBurn (short/long), открытый инцидент и историю.
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

	// HasData=false — график и проценты не рендерятся, экран не падает.
	var sb2 strings.Builder
	empty := SLODetailVM{ProjectID: 1, ID: 6, Name: "empty slo", Kind: "uptime", TargetPct: 99.9, WindowDays: 30, HasData: false, Chart: templ.NopComponent}
	if err := SLODetailScreen(empty, "u@example.com").Render(ctx, &sb2); err != nil {
		t.Fatalf("Render empty: %v", err)
	}
	if !strings.Contains(sb2.String(), "empty slo") {
		t.Errorf("пустой SLODetailScreen не содержит имя")
	}
}
