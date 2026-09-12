package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestActionRowFormSelectorsAreDirectChild(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("readAppCSS: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	for _, container := range []string{".issue-actions", ".card-toolbar", ".card-toolbar-group"} {
		re := regexp.MustCompile(regexp.QuoteMeta(container) + `\s+form\s*[,{]`)
		if loc := re.FindString(css); loc != "" {
			t.Errorf("app.css: селектор %q задаёт раскладку любому вложенному <form> — "+
				"он протечёт в форму внутри <details class=\"dropdown-control\"> и разложит "+
				"её вертикальные поля в строку (живой случай v0.22.0). Нужен прямой потомок: %s > form",
				strings.TrimSuffix(loc, "{"), container)
		}
	}
}
