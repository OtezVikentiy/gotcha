package export

import (
	"context"
	"fmt"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

type IssueSource interface {
	Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error
}

func IssueColumns() []string {
	return []string{"id", "title", "culprit", "level", "status", "times_seen",
		"first_seen", "last_seen", "environments", "assignee_email", "url"}
}

type issueSource struct {
	svc     *issue.Service
	baseURL string
}

// Ссылка в выгрузке абсолютная — открывается из почты и таблицы, относительная там бесполезна.
func NewIssueSource(svc *issue.Service, baseURL string) IssueSource {
	return &issueSource{svc: svc, baseURL: strings.TrimRight(baseURL, "/")}
}

// Путь — ровно как в роутере (GET /issues/{id}), не /projects/{id}/issues/{id}: такого маршрута нет.
func IssueURL(baseURL string, issueID int64) string {
	return fmt.Sprintf("%s/issues/%d", strings.TrimRight(baseURL, "/"), issueID)
}

// assignee_email маскируется тем же MaskUser, что user_email/user_ip в источнике событий.
func (s *issueSource) Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error {
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
		return fn(s.toRecord(it, includePII))
	})
}

func (s *issueSource) toRecord(it issue.Issue, includePII bool) Record {
	assigneeEmail := it.AssigneeEmail
	if !includePII {
		_, assigneeEmail = MaskUser("", assigneeEmail)
	}
	return Record{
		"id":             it.ID,
		"title":          it.Title,
		"culprit":        it.Culprit,
		"level":          it.Level,
		"status":         it.Status,
		"times_seen":     it.TimesSeen,
		"first_seen":     it.FirstSeen,
		"last_seen":      it.LastSeen,
		"environments":   strings.Join(it.Environments, ", "),
		"assignee_email": assigneeEmail,
		"url":            IssueURL(s.baseURL, it.ID),
	}
}
