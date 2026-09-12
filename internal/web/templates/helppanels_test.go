package templates

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

func TestHelpPanelOnRemainingSections(t *testing.T) {
	now := time.Now()
	pages := map[string]struct {
		html  string
		guide string
	}{
		"dependencies": {renderTo(t, DependenciesScreen(7, nil, DepsFilter{Range: TimeRangeVM{Key: "24h"}}, stub(), false, false, "u@e.com")), "/docs/dependencies"},
		"deployments":  {renderTo(t, DeploymentsScreen(7, nil, "u@e.com")), "/docs/deployments"},
		"logs":         {renderTo(t, LogsScreen(7, []LogRow{NewLogRow(log.LogRow{Timestamp: now, Severity: "ERROR", Body: "boom"})}, LogsFilter{Range: TimeRangeVM{Key: "24h"}}, false, "", LogsHistogram{Empty: true}, LogFacets{}, "u@e.com", LogSavedFiltersPanel{}, "")), "/docs/logs"},
		"exports":      {renderTo(t, Exports(7, nil, true, "u@e.com", true, "", nil)), "/docs/exports"},
	}
	for name, p := range pages {
		if !strings.Contains(p.html, `class="help-panel"`) {
			t.Errorf("%s: нет свёрнутой справки help-panel", name)
		}
		if !strings.Contains(p.html, `href="`+p.guide+`"`) {
			t.Errorf("%s: справка не ведёт в гайд %s", name, p.guide)
		}
		if !strings.Contains(p.html, "Что это за раздел?") {
			t.Errorf("%s: у справки нет заголовка help.<area>.title", name)
		}
	}
}
