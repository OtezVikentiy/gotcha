package guards

import (
	"regexp"
	"strings"
	"testing"
)

// monitorHeadersFuncHeaderRe — заголовок функции (метод Handler ИЛИ обычная
// функция пакета web) вида "func (h *Handler) Name(" / "func Name(" в начале
// строки: тем же приёмом, что и channelsDoorFuncHeaderRe, узнаём, В КАКОЙ
// функции найден интересующий вызов. Расширен по сравнению с
// channelsDoorFuncHeaderRe, потому что monitorFormFromMonitor — свободная
// функция пакета, а не метод *Handler.
var monitorHeadersFuncHeaderRe = regexp.MustCompile(`^func (?:\([^)]*\)\s*)?(\w+)\(`)

// TestMonitorHeadersGoThroughOneSite — сорс-guard находки P1-3/B (волна 5):
// у каналов есть отдельная дверь-функция (channelsForView,
// channels_door_test.go), которая маскирует секрет ДО того, как он попадёт в
// структуру формы. У заголовков монитора такой отдельной функции нет — они
// живут внутри Config (JSON) конкретного монитора, а не за отдельным
// аксессором, поэтому классический door-guard (одна функция-дверь + allowlist
// легитимных исключений) сюда не ложится один в один.
//
// Вместо этого проверяем структурный инвариант, который и делает
// renderMonitorForm/monitorEditPage безопасными по факту (см. monitorform.go):
//
//  1. Сырые значения заголовков сохранённого монитора попадают в MonitorFormData
//     ровно в одном месте — вызове headersToText(c.Headers) внутри
//     monitorFormFromMonitor. Другого места, которое читало бы c.Headers из
//     реального конфига монитора и клало бы его текстом в форму, быть не
//     должно: появление второго такого места — это потенциально новый рендер-
//     сайт, который забудет про маскировку (ровно тот сценарий, который эту
//     находку и породил).
//  2. Единственный вызывающий monitorFormFromMonitor — monitorEditPage.
//     Появление второго вызывающего означает новую страницу/хендлер, которая
//     получает сырые заголовки в форму и должна (но сторож этого не видит)
//     сама решить про маскировку.
//  3. maskHeaderValues тоже вызывается ровно один раз — и именно из
//     monitorEditPage, той же функции, что и monitorFormFromMonitor. Это и
//     есть замена "двери": место, где заголовки становятся текстом формы, и
//     место, где они маскируются для оператора, — одна и та же функция, а не
//     разнесённые по файлу шаги, которые легко рассинхронизировать.
//
// Если тест покраснел — либо кто-то завёл второй сайт чтения сырых
// заголовков (нужно решить, маскируется ли он для !canManage — тест не
// проверяет ЭТО, но проверяет, что вопрос не пройдёт незамеченным), либо
// переименовал одну из трёх функций, и константы ниже нужно поправить вместе
// с ревью diff'а, а не молча.
//
// Дополняет (не заменяет) поведенческий тест
// TestWebMonitorEditOperatorMasksHeaderValues
// (internal/web/monitorform_headers_test.go): тот проверяет РЕЗУЛЬТАТ (сырого
// секрета нет в HTML у оператора), этот — что результат не может тихо
// перестать проверяться из-за нового рендер-сайта.
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
