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

// дублируется в internal/web/templates: импортировать оттуда internal/web нельзя (цикл).
// сравниваем на точное равенство, не на подстроку, чтобы копии не разошлись незаметно.
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
		true,
		baseURL, "u@example.com", false,
	).Render(ctx, &sb)
	if err != nil {
		t.Fatalf("render MonitorDetail: %v", err)
	}
	html := sb.String()

	templatePing, templateSnippet := extractHeartbeatSnippetsFromHTML(t, html)

	if webPing := heartbeatPingURL(baseURL, token); templatePing != webPing {
		t.Errorf("копии heartbeatPingURL разошлись:\n  web-пакет:      %q\n  templates-копия: %q",
			webPing, templatePing)
	}
	if templateSnippet != webSnippet {
		t.Errorf("копии heartbeatCronSnippet разошлись:\n  web-пакет:      %q\n  templates-копия: %q",
			webSnippet, templateSnippet)
	}
}

// Снимает HTML-экранирование, которое templ применяет к текстовому узлу
// (например ">" в "curl ... >/dev/null" приходит как "&gt;").
func extractHeartbeatSnippetsFromHTML(t *testing.T, html string) (ping, cron string) {
	t.Helper()
	const openTag, closeTag = `<pre class="copy-preview">`, "</pre>"

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
			t.Fatalf("незакрытый <pre class=\"copy-preview\"> в рендере MonitorDetail")
		}
		blocks = append(blocks, rest[:j])
		rest = rest[j+len(closeTag):]
	}
	if len(blocks) != 2 {
		t.Fatalf("в карточке heartbeat найдено %d copy-блоков, want 2 (ping URL + cron-сниппет): %v", len(blocks), blocks)
	}

	ping = htmlUnescapeMinimal(blocks[0])
	if !strings.HasPrefix(ping, "http") {
		t.Fatalf("первый copy-блок не похож на ping URL: %q", ping)
	}
	cron = htmlUnescapeMinimal(blocks[1])
	if !strings.Contains(cron, "curl") {
		t.Fatalf("второй copy-блок не похож на cron-сниппет: %q", cron)
	}
	return ping, cron
}

// Не html.UnescapeString: раскрывает только то, что реально может прийти от
// templ.EscapeString, чтобы сторож ловил расхождение, а не маскировал его широким раскрывателем.
func htmlUnescapeMinimal(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&#34;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	return s
}
