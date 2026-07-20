package web_test

import (
	"os"
	"regexp"
	"testing"
)

// cssClassesInMarkup — классы, которые шаблоны ставят в разметку и которые
// обязаны иметь определение в app.css. Неопределённый класс не ломает ни
// сборку, ни тесты: блок просто рендерится без стилей и на глаз выглядит
// «пресно», а не сломано, — поэтому страж и нужен.
//
// Список ведётся вручную и адресно: полный автоматический разбор всех
// class="..." даёт много шума от динамических хелперов вида
// levelBadgeClass()/statusBadgeClass(), которые собирают имя класса в Go.
// Пополняется по мере переодевания областей (под-проект ④): сейчас закрыта
// область Issues, Performance/Trace добавляются своей задачей.
var cssClassesInMarkup = []string{
	// Issues (issuedetail.templ)
	"issue-detail",
	"issue-chart",
	"issue-events-heading",
	"event-trace-link",
	"tags",

	// Performance / Trace / Profiles
	"endpoint-detail",
	"endpoint-chart",
	"web-vitals-panel",
	// perf-issue-detail сюда не входит: он стоит в паре с issue-detail,
	// который и несёт раскладку страницы, — это семантический маркер, а не
	// стилевой хук (как и классы *-form, см. комментарий в app.css).
	"perf-evidence",
	"perf-evidence-list",
	"perf-urls",
	"perf-sample-trace",
	"trace-detail",
	"trace-waterfall",
	"trace-flame",
	"trace-actions",
	"flamegraph-wrap",
	"metric-chart-wrap",

	// Публичная status-страница
	"status-body",
	"status-incidents",
	"status-maintenance",

	"inline",
}

func TestNoUndefinedCSSClasses(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	sheet := string(css)

	for _, c := range cssClassesInMarkup {
		re := regexp.MustCompile(`\.` + regexp.QuoteMeta(c) + `\b`)
		if !re.MatchString(sheet) {
			t.Errorf("класс %q используется в разметке, но не определён в app.css", c)
		}
	}
}
