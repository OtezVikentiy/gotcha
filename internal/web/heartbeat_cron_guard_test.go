package web

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// heartbeatCronSnippet живёт в двух независимых копиях, потому что
// internal/web/templates не может импортировать internal/web (цикл:
// web -> templates -> web) — см. докблок heartbeatPingURL/heartbeatCronSnippet
// в monitorform.go и templates/webhelpers.go. TestHeartbeatSnippets (этот
// пакет) и TestHeartbeatMonitorDetail (templates) проверяли КАЖДУЮ копию по
// отдельности на вхождение одной и той же подстроки ("curl -fsS -X POST ") —
// но копии между собой ни разу не сравнивались: правка ОДНОЙ из них
// (например, потеря "-fsS" в шаблоне) проходила оба теста молча, раз обе
// подстроки всё ещё "содержались" каждая в своём рендере/выхлопе.
//
// Этот сторож — по прецеденту internal/guards/agent_env_contract_test.go —
// рендерит настоящий templates.MonitorDetail с фиксированными
// baseURL/token/интервалом, вытаскивает cron-сниппет из готового HTML и
// сравнивает его на ТОЧНОЕ РАВЕНСТВО со строкой, которую вернула
// heartbeatCronSnippet этого пакета. Не "обе содержат подстроку" — точное
// равенство, иначе сторож повторит нынешнюю слепоту: строка из шаблона может
// расходиться с web-копией в любом другом месте (без "-fsS", с GET вместо
// POST, с иным приглашением cron) и тест всё равно останется зелёным, если
// проверять только общую подстроку.
func TestHeartbeatCronSnippetMatchesTemplateCopy(t *testing.T) {
	const baseURL = "https://gotcha.example"
	const token = "hbtok-guard"
	const intervalSeconds = 300

	webSnippet := heartbeatCronSnippet(baseURL, token, intervalSeconds)

	m := uptime.Monitor{
		ID:              99,
		Name:            "guard",
		Kind:            uptime.KindHeartbeat,
		Enabled:         true,
		IntervalSeconds: intervalSeconds,
		HeartbeatToken:  token,
	}
	stat := uptime.UptimeStat{}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	err := templates.MonitorDetail(
		m, "up", stat, stat, stat,
		templ.Raw("<svg data-guard-chart></svg>"),
		templates.TimeRangeVM{Key: "24h"},
		nil, nil, 1, 0,
		true, true,
		baseURL, "u@example.com",
	).Render(ctx, &sb)
	if err != nil {
		t.Fatalf("render MonitorDetail: %v", err)
	}
	html := sb.String()

	templateSnippet := extractCronSnippetFromHTML(t, html)

	if templateSnippet != webSnippet {
		t.Errorf("копии heartbeatCronSnippet разошлись:\n  web-пакет:      %q\n  templates-копия: %q",
			webSnippet, templateSnippet)
	}
}

// extractCronSnippetFromHTML достаёт содержимое второго <code>...</code> в
// карточке heartbeat (первый — ping URL, второй — cron-сниппет, см.
// monitordetail.templ) и снимает HTML-экранирование, которое templ применяет
// к текстовому узлу (сниппет содержит ">" в "curl ... >/dev/null", в HTML это
// "&gt;").
func extractCronSnippetFromHTML(t *testing.T, html string) string {
	t.Helper()
	const openTag, closeTag = "<code>", "</code>"

	var blocks []string
	rest := html
	for {
		i := strings.Index(rest, openTag)
		if i < 0 {
			break
		}
		rest = rest[i+len(openTag):]
		j := strings.Index(rest, closeTag)
		if j < 0 {
			t.Fatalf("незакрытый <code> в рендере MonitorDetail")
		}
		blocks = append(blocks, rest[:j])
		rest = rest[j+len(closeTag):]
	}
	if len(blocks) != 2 {
		t.Fatalf("в карточке heartbeat найдено %d блоков <code>, want 2 (ping URL + cron-сниппет): %v", len(blocks), blocks)
	}

	snippet := htmlUnescapeMinimal(blocks[1])
	if !strings.Contains(snippet, "curl") {
		t.Fatalf("второй <code>-блок не похож на cron-сниппет: %q", snippet)
	}
	return snippet
}

// htmlUnescapeMinimal раскрывает ровно те HTML-сущности, которые templ может
// вставить в текстовый узел с cron-командой ("&gt;" из "curl ... >/dev/null"
// плюс парный "&amp;" на случай будущих правок сниппета) — не общего
// назначения html.UnescapeString, чтобы сторож ловил именно то, что реально
// приходит из templ.EscapeString, а не маскировал расхождение через более
// широкий раскрыватель сущностей.
func htmlUnescapeMinimal(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&#34;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	return s
}
