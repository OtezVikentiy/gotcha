package templates

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// TestRelativeTimeCarriesExactMoment: относительная подпись отвечает на
// вопрос «давно ли», но дежурному нужен точный момент, чтобы сопоставить
// проблему с логами приложения. Раньше точное время добывалось только через
// «Сырой JSON события» — на списке issues его не было вовсе.
//
// UTC намеренно: настройки пояса у пользователя нет, а в аудите доставок UTC
// уже принят.
func TestRelativeTimeCarriesExactMoment(t *testing.T) {
	moment := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	rows := []IssueRow{
		{Issue: issue.Issue{ID: 1, Title: "boom", Level: "error", Status: "unresolved", LastSeen: moment}, Sparkline: stub()},
	}
	out := renderTo(t, IssuesList(7, rows, IssuesFilter{}, 1, 1, "u@e.com", nil, nil, GettingStartedVM{}, false))
	if !strings.Contains(out, "<time ") {
		t.Fatal("относительное время без <time>: точный момент недоступен")
	}
	if !strings.Contains(out, `datetime="2026-07-20T10:00:00Z"`) {
		t.Fatalf("нет машинночитаемого момента в datetime: %s", out)
	}
}
