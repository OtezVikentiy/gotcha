package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

const defaultMaxIssueIDsForEventExport = 20000

// Отдельный потолок от GOTCHA_EXPORT_MAX_ROWS — подстраховка, если раннер не остановит поток.
const eventStreamSafetyLimit = 1_000_000

var ErrTooManyIssues = errors.New("экспорт: фильтр резолвится в слишком много групп, сузьте условия")

// При maxIssueIDs<=0 IDsForFilter уходит в LIMIT 1: любой результат ложно читается как overflow.
var ErrMaxIssueIDsNotConfigured = errors.New("экспорт: eventSource собран без потолка id групп (используйте NewEventSource)")

type EventSource interface {
	Stream(ctx context.Context, projectID, scopeIssueID int64, includePII bool, p Params, fn func(Record) error) error
}

// Stacktrace/contexts/breadcrumbs/request сюда не входят — в CSV они дают нечитаемое полотно.
// В Record поля остаются всегда, CSV-писатель берёт только эти колонки.
func EventColumns() []string {
	return []string{"timestamp", "event_id", "issue_id", "level", "message",
		"exception_type", "exception_value", "environment", "release", "server_name",
		"sdk", "trace_id", "user_id", "user_ip", "user_email", "tags"}
}

type eventSource struct {
	q           *event.Query
	issues      *issue.Service
	maxIssueIDs int
}

// Заявка на PII — свойство конкретного вызова, не источника: один eventSource в Worker
// обслуживает заявки с разным значением галки одну за другой.
func NewEventSource(q *event.Query, issues *issue.Service) EventSource {
	return &eventSource{q: q, issues: issues, maxIssueIDs: defaultMaxIssueIDsForEventExport}
}

// Группы идут в порядке IDsForFilter (last_seen DESC), внутри — timestamp DESC:
// усечение по лимиту отбрасывает наименее активные группы, не произвольные.
func (s *eventSource) Stream(ctx context.Context, projectID, scopeIssueID int64, includePII bool, p Params, fn func(Record) error) error {
	issueIDs, err := s.resolveIssueIDs(ctx, projectID, scopeIssueID, p)
	if err != nil {
		return err
	}
	if len(issueIDs) == 0 {
		return nil
	}
	// Salt — на один вызов Stream: PseudonymizeUserID даёт одинаковый псевдоним внутри
	// выгрузки и разный между ними, только пока salt не переживает вызов.
	salt := NewExportSalt()
	return s.q.StreamForExport(ctx, projectID, issueIDs, p.Since, p.Until, eventStreamSafetyLimit, func(ev event.Stored) error {
		return fn(s.toRecord(ev, includePII, salt))
	})
}

// buildIssueFilter уже применяет issues.last_seen >= p.Since — дублировать отсечку
// здесь не нужно и бессмысленно.
func (s *eventSource) resolveIssueIDs(ctx context.Context, projectID, scopeIssueID int64, p Params) ([]int64, error) {
	if scopeIssueID != 0 {
		return []int64{scopeIssueID}, nil
	}
	if s.maxIssueIDs <= 0 {
		return nil, ErrMaxIssueIDsNotConfigured
	}

	f := issue.Filter{
		Status:      p.Status,
		Level:       p.Level,
		Query:       p.Query,
		Sort:        p.Sort,
		Environment: p.Environment,
		Since:       p.Since,
		Until:       p.Until,
	}
	ids, overflow, err := s.issues.IDsForFilter(ctx, projectID, f, s.maxIssueIDs)
	if err != nil {
		return nil, fmt.Errorf("экспорт событий: резолв групп: %w", err)
	}
	if overflow {
		return nil, ErrTooManyIssues
	}
	return ids, nil
}

// stacktrace/breadcrumbs обязаны маскироваться через MaskJSON как request/contexts —
// иначе frame-vars и URL из breadcrumbs утекают даже в выгрузку «без ПДн».
func (s *eventSource) toRecord(ev event.Stored, includePII bool, salt []byte) Record {
	userIP, userEmail := ev.UserIP, ev.UserEmail
	userID := ev.UserID
	message, exceptionValue := ev.Message, ev.ExceptionValue
	request, contexts := ev.Request, ev.Contexts
	stacktrace, breadcrumbs := ev.Stacktrace, ev.Breadcrumbs
	tags := ev.Tags
	if !includePII {
		userIP, userEmail = MaskUser(userIP, userEmail)
		userID = PseudonymizeUserID(userID, salt)
		message = MaskMessage(message)
		exceptionValue = MaskMessage(exceptionValue)
		request = MaskJSON(request)
		contexts = MaskJSON(contexts)
		stacktrace = MaskJSON(stacktrace)
		breadcrumbs = MaskJSON(breadcrumbs)
		tags = MaskTags(tags)
	}
	return Record{
		"timestamp":       ev.Timestamp,
		"event_id":        ev.ID,
		"issue_id":        ev.IssueID,
		"level":           ev.Level,
		"message":         message,
		"exception_type":  ev.ExceptionType,
		"exception_value": exceptionValue,
		"environment":     ev.Environment,
		"release":         ev.Release,
		"server_name":     ev.ServerName,
		"sdk":             ev.SDK,
		"trace_id":        ev.TraceID,
		"user_id":         userID,
		"user_ip":         userIP,
		"user_email":      userEmail,
		"tags":            flattenTags(tags),
		"stacktrace":      rawJSON(stacktrace),
		"contexts":        rawJSON(contexts),
		"breadcrumbs":     rawJSON(breadcrumbs),
		"request":         rawJSON(request),
	}
}

// Сортировка ключей нужна: map обходится в случайном порядке, иначе строка была бы нестабильной.
func flattenTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + tags[k]
	}
	return strings.Join(parts, "; ")
}

// Пустая строка — «поле не пришло», не «пришёл пустой JSON»:
// null в выгрузке отличим от {}, которое событие могло прислать явно.
func rawJSON(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}
