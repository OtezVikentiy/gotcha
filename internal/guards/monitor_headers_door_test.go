package guards

import (
	"regexp"
	"strings"
	"testing"
)

var monitorHeadersFuncHeaderRe = regexp.MustCompile(`^func (?:\([^)]*\)\s*)?(\w+)\(`)

// Заголовков монитора нет за отдельным аксессором — вместо door-функции проверяется,
// что чтение сырых значений и их маскировка остаются в одной функции.
func TestMonitorHeadersGoThroughOneSite(t *testing.T) {
	const (
		wantReadFunc = "monitorFormFromMonitor"
		wantCallFunc = "monitorEditPage"
		wantMaskFunc = "monitorEditPage"
	)

	tree := Load(t)

	type hit struct {
		path string
		line int
		fn   string
	}
	var readSites, callSites, maskSites []hit

	for _, f := range tree.GoFiles {
		if !strings.HasPrefix(f.Path, "internal/web/") || f.Generated || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		currentFunc := ""
		for i, line := range strings.Split(f.Body, "\n") {
			if m := monitorHeadersFuncHeaderRe.FindStringSubmatch(line); m != nil {
				currentFunc = m[1]
			}
			checked := stripTrailingComment(line)
			trimmed := strings.TrimSpace(checked)
			isDef := strings.HasPrefix(trimmed, "func ")

			if strings.Contains(checked, "headersToText(c.Headers)") {
				readSites = append(readSites, hit{f.Path, i + 1, currentFunc})
			}
			if !isDef && strings.Contains(checked, "monitorFormFromMonitor(") {
				callSites = append(callSites, hit{f.Path, i + 1, currentFunc})
			}
			if !isDef && strings.Contains(checked, "maskHeaderValues(") {
				maskSites = append(maskSites, hit{f.Path, i + 1, currentFunc})
			}
		}
	}

	if len(readSites) != 1 {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: headersToText(c.Headers) найден %d раз(а), ожидался ровно 1 — "+
			"новый сайт чтения сырых сохранённых заголовков монитора должен сам решить вопрос маскировки для "+
			"оператора (P1-3/B, волна 5): %v", len(readSites), readSites)
	} else if readSites[0].fn != wantReadFunc {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: headersToText(c.Headers) переехал из %s в %s (%s:%d) — "+
			"поправьте константу wantReadFunc в этом тесте вместе с ревью diff'а",
			wantReadFunc, readSites[0].fn, readSites[0].path, readSites[0].line)
	}

	if len(callSites) != 1 {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: monitorFormFromMonitor(...) вызван %d раз(а), ожидался ровно 1 — "+
			"новый вызывающий получает сырые заголовки монитора в форму и должен сам маскировать их для "+
			"оператора (P1-3/B, волна 5): %v", len(callSites), callSites)
	} else if callSites[0].fn != wantCallFunc {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: monitorFormFromMonitor(...) теперь вызывается из %s, а не %s (%s:%d) — "+
			"поправьте константу wantCallFunc вместе с ревью diff'а",
			callSites[0].fn, wantCallFunc, callSites[0].path, callSites[0].line)
	}

	if len(maskSites) != 1 {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: maskHeaderValues(...) вызван %d раз(а), ожидался ровно 1 — "+
			"маскировка должна оставаться единственной и явной (P1-3/B, волна 5): %v", len(maskSites), maskSites)
	} else if maskSites[0].fn != wantMaskFunc {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: maskHeaderValues(...) теперь вызывается из %s, а не %s (%s:%d) — "+
			"поправьте константу wantMaskFunc вместе с ревью diff'а",
			maskSites[0].fn, wantMaskFunc, maskSites[0].path, maskSites[0].line)
	}

	if len(callSites) == 1 && len(maskSites) == 1 && callSites[0].fn != maskSites[0].fn {
		t.Errorf("TestMonitorHeadersGoThroughOneSite: чтение сырых заголовков (%s) и маскировка (%s) разъехались по "+
			"разным функциям — раньше это была одна и та же monitorEditPage, теперь их проще рассинхронизировать",
			callSites[0].fn, maskSites[0].fn)
	}
}
