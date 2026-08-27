package web

import (
	"regexp"
	"strings"
	"testing"
)

// Сторож раскладки форм внутри раскрывающихся кнопок (<details
// class="dropdown-control">).
//
// Живой случай (приёмка v0.22.0 в браузере): правило `.issue-actions form`
// задавало display:flex ЛЮБОМУ вложенному <form>, а не только строке кнопок
// статуса — и ловило форму экспорта, приехавшую внутрь dropdown-control на
// той же странице. Поля вертикальной формы раскладывались в строку внутри
// max-width:280px: селект формата обрезан, подсказка PII — колонка в одно
// слово, кнопка — вертикальная плита. Ни один тест этого не видел: templ-тесты
// проверяют разметку, CSS в них не применяется, а браузерная приёмка до того
// момента гоняла только список ошибок, где обёртка другая.
//
// Правило простое: селектор, задающий раскладку форме на странице ошибки или
// в списке кнопок экспорта, обязан быть по ПРЯМОМУ потомку. Иначе он
// протекает во всё, что окажется вложено внутрь, — и протечка видна только
// глазами в браузере.
func TestActionRowFormSelectorsAreDirectChild(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("readAppCSS: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	// Контейнеры, внутри которых живёт раскрывающаяся форма (dropdown-control).
	for _, container := range []string{".issue-actions", ".issues-export-actions"} {
		re := regexp.MustCompile(regexp.QuoteMeta(container) + `\s+form\s*[,{]`)
		if loc := re.FindString(css); loc != "" {
			t.Errorf("app.css: селектор %q задаёт раскладку любому вложенному <form> — "+
				"он протечёт в форму внутри <details class=\"dropdown-control\"> и разложит "+
				"её вертикальные поля в строку (живой случай v0.22.0). Нужен прямой потомок: %s > form",
				strings.TrimSuffix(loc, "{"), container)
		}
	}
}
