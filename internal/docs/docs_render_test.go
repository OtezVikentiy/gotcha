package docs

import (
	"bytes"
	"strings"
	"testing"
)

// TestDocsTablesWrappedInScrollRegion: таблицы документации рендерятся в ту же
// скролл-обёртку, что scrollRegion в шаблонах (№31/№75): role=table
// возвращается скринридеру (display:block с таблицы снят в app.css), а
// прокрутка и клавиатурный доступ живут на обёртке.
func TestDocsTablesWrappedInScrollRegion(t *testing.T) {
	src := []byte("| a | b |\n|---|---|\n| 1 | 2 |\n")
	for _, loc := range []string{"ru", "en"} {
		var buf bytes.Buffer
		if err := mdFor(loc).Convert(src, &buf); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.Contains(out, `<div class="table-scroll" tabindex="0" role="region" aria-label="`) {
			t.Errorf("[%s] таблица не обёрнута в скролл-регион: %s", loc, out)
		}
		if !strings.Contains(out, "</table></div>") {
			t.Errorf("[%s] обёртка не закрыта: %s", loc, out)
		}
	}
}
