package web

import (
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Сторож <meta name="theme-color"> (layout.templ) против --bg в app.css:
// цвет системной рамки браузера обязан совпадать с полотном темы, иначе над
// страницей висит полоса чужого цвета. Истина — таблица стилей; шаблон
// держит копию, потому что <meta> не умеет читать CSS-переменные.
func TestThemeColorMatchesCSS(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")
	bgRe := regexp.MustCompile(`--bg:\s*(#[0-9a-fA-F]{6})\s*;`)

	blocks := map[string]*regexp.Regexp{
		"dark":  regexp.MustCompile(`(?m)^:root \{`),
		"light": regexp.MustCompile(`(?m)^:root\[data-theme="light"\] \{`),
	}
	for code, open := range blocks {
		loc := open.FindStringIndex(css)
		if loc == nil {
			t.Fatalf("%s: в app.css нет блока %s", code, open)
		}
		body := css[loc[1]:]
		if end := strings.Index(body, "}"); end >= 0 {
			body = body[:end]
		}
		m := bgRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: в блоке %s нет --bg", code, open)
		}
		if got := templates.ThemeColor(code); !strings.EqualFold(got, m[1]) {
			t.Errorf("%s: theme-color в layout.templ = %q, а --bg в app.css = %q", code, got, m[1])
		}
	}
	if got := templates.ThemeColor("system"); got != "" {
		t.Errorf("для «system» единого цвета нет (две <meta> с media), получено %q", got)
	}
}
