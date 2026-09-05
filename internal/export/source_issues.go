package export

import (
	"context"
	"fmt"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// IssueSource — источник строк выгрузки kind=issues: превращает группы
// проекта, отданные keyset-обходом issue.Service.StreamForExport, в Record
// для писателя (задача 4). assignee_email — прямой идентификатор
// пользователя (email назначенного), как user_email в выгрузке событий:
// он маскируется тем же MaskUser при includePII == false. Остальные
// колонки групп (§6 спеки) прямых идентификаторов не содержат.
type IssueSource interface {
	Stream(ctx context.Context, projectID int64, includePII bool, p Params, fn func(Record) error) error
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

// IssueURL — значение колонки url выгрузки групп: абсолютная ссылка на
// страницу группы. Путь — ровно тот, что зарегистрирован роутером
// («GET /issues/{id}», internal/web/web.go; тот же, что строит
// web.issueDetailPath), а НЕ производный от проекта: страницы вида
// /projects/{id}/issues/{id} не существует, и v0.22.0 уехала в прод с
// колонкой url, каждая строка которой вела в 404 (приёмка в браузере).
// Ошибку не поймали тесты, потому что ожидание собиралось тем же
// fmt.Sprintf, что и реализация; сторож против повторения —
// TestExportIssueURLHitsRegisteredRoute (internal/web), он спрашивает
// сам роутер, а не повторяет строку.
func IssueURL(baseURL string, issueID int64) string {
	return fmt.Sprintf("%s/issues/%d", strings.TrimRight(baseURL, "/"), issueID)
}

// Stream переносит Params заявки в issue.Filter (те же имена полей — Params
// снимает фильтр списка issues на момент постановки заявки в очередь) и
// стримит группы дальше как Record. includePII == false маскирует
// assignee_email тем же MaskUser, что и user_email/user_ip в источнике
// событий (source_events.go).
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

// toRecord — маппинг issue.Issue в Record выгрузки issues (§6 спеки), тот же
// набор колонок, что и IssueColumns(). Вынесено из Stream отдельным методом
// ради теста контракта K4-7 аудита (TestExportContract* в
// contract_version_test.go): тест обязан прогнать РЕАЛЬНЫЙ маппинг на
// фикстуре issue.Issue, а не воспроизводить эту логику собственной копией.
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
