package export

import (
	"context"
	"fmt"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// IssueSource — источник строк выгрузки kind=issues: превращает группы
// проекта, отданные keyset-обходом issue.Service.StreamForExport, в Record
// для писателя (задача 4). PII-маскирование сюда не относится: колонки
// групп (§6 спеки) не содержат прямых идентификаторов пользователя —
// они есть только у источника событий.
type IssueSource interface {
	Stream(ctx context.Context, projectID int64, p Params, fn func(Record) error) error
}

// IssueColumns — порядок колонок выгрузки issues, одинаковый для всех
// форматов: JSON/NDJSON пишут Record целиком и порядок игнорируют, но CSV
// без явного списка колонок не знает, в каком порядке класть значения.
func IssueColumns() []string {
	return []string{"id", "title", "culprit", "level", "status", "times_seen",
		"first_seen", "last_seen", "environments", "assignee_email", "url"}
}

type issueSource struct {
	svc     *issue.Service
	baseURL string
}

// NewIssueSource создаёт источник групп. baseURL — адрес инстанса без
// хвостового слэша: выгрузку открывают в почте и в таблице, относительная
// ссылка на группу там бесполезна.
func NewIssueSource(svc *issue.Service, baseURL string) IssueSource {
	return &issueSource{svc: svc, baseURL: strings.TrimRight(baseURL, "/")}
}

// Stream переносит Params заявки в issue.Filter (те же имена полей — Params
// снимает фильтр списка issues на момент постановки заявки в очередь) и
// стримит группы дальше как Record.
func (s *issueSource) Stream(ctx context.Context, projectID int64, p Params, fn func(Record) error) error {
	f := issue.Filter{
		Status:      p.Status,
		Level:       p.Level,
		Query:       p.Query,
		Sort:        p.Sort,
		Environment: p.Environment,
		Since:       p.Since,
		Until:       p.Until,
	}
	return s.svc.StreamForExport(ctx, projectID, f, func(it issue.Issue) error {
		return fn(Record{
			"id":             it.ID,
			"title":          it.Title,
			"culprit":        it.Culprit,
			"level":          it.Level,
			"status":         it.Status,
			"times_seen":     it.TimesSeen,
			"first_seen":     it.FirstSeen,
			"last_seen":      it.LastSeen,
			"environments":   strings.Join(it.Environments, ", "),
			"assignee_email": it.AssigneeEmail,
			"url":            fmt.Sprintf("%s/projects/%d/issues/%d", s.baseURL, projectID, it.ID),
		})
	})
}
