package guards

import (
	"regexp"
	"strings"
	"testing"
)

// Три <nav class="tabs"> на страницах производительности были безымянными:
// скринридер объявляет «навигация» без ответа «какая» (№83, WCAG 1.3.1/2.4.6).
// Ориентир-навигация обязана нести aria-label.
var tabsNavRe = regexp.MustCompile(`<nav[^>]*class="tabs"[^>]*>`)

func TestTabsNavsAreLabelled(t *testing.T) {
	tree := Load(t)
	for _, f := range tree.Templates {
		for _, m := range tabsNavRe.FindAllString(f.Body, -1) {
			if !strings.Contains(m, "aria-label") {
				t.Errorf("%s: безымянный ориентир %s", f.Path, m)
			}
		}
	}
}
