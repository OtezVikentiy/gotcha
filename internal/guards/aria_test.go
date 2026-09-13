package guards

import (
	"regexp"
	"strings"
	"testing"
)

// class="tabs" сматчится и как "tabs compact" — граница \b, не точное значение
// атрибута, иначе составной класс проходил бы сторож не заметив его вовсе.
var tabsNavRe = regexp.MustCompile(`<nav[^>]*class="[^"]*\btabs\b[^"]*"[^>]*>`)

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
