package guards

import (
	"regexp"
	"strings"
	"testing"
)

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
