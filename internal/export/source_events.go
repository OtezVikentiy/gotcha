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

// defaultMaxIssueIDsForEventExport — потолок числа групп, резолвящихся из
// фильтра PostgreSQL перед выгрузкой событий по проекту (§8 спеки: 20 000
// id). Значение по умолчанию для поля eventSource.maxIssueIDs, проставляемое
// в NewEventSource: потолок — поле структуры, а не пакетная переменная,
// чтобы тест на отказ не делил изменяемое глобальное состояние с остальными
// тестами пакета (мина под будущий t.Parallel()) — вместо этого тест
// собирает eventSource напрямую с нужным значением поля.
const defaultMaxIssueIDsForEventExport = 20000

// eventStreamSafetyLimit — защитный потолок числа строк одного обхода CH,
// независимый от настраиваемого GOTCHA_EXPORT_MAX_ROWS: тот применяет
// раннер задачи, останавливая обход возвратом ошибки из fn при достижении
// лимита заявки. Здесь — подстраховка на случай, если раннер почему-то не
// остановит поток: без верхней границы ORDER BY issue_id, timestamp DESC не
// помешал бы ClickHouse материализовать весь набор целиком.
const eventStreamSafetyLimit = 1_000_000

// ErrTooManyIssues возвращается, когда фильтр выгрузки событий резолвится в
// число групп больше eventSource.maxIssueIDs. Обрезать список молча нельзя:
// какие именно группы выпали бы, пользователь узнать не может, а молча
// неполная выгрузка хуже отсутствующей (см. §8 спеки).
var ErrTooManyIssues = errors.New("экспорт: фильтр резолвится в слишком много групп, сузьте условия")

// ErrMaxIssueIDsNotConfigured возвращается, если eventSource собран в обход
// NewEventSource с нулевым или отрицательным maxIssueIDs. Без этой проверки
// IDsForFilter(..., 0) уходит в LIMIT 1, и любой непустой результат тут же
// читался бы как overflow=true — ошибка конструирования маскировалась бы
// под настоящий ErrTooManyIssues, хотя дело не в фильтре, а в забытом потолке.
var ErrMaxIssueIDsNotConfigured = errors.New("экспорт: eventSource собран без потолка id групп (используйте NewEventSource)")

// EventSource — источник строк выгрузки kind=events.
type EventSource interface {
	Stream(ctx context.Context, projectID, scopeIssueID int64, includePII bool, p Params, fn func(Record) error) error
}

// EventColumns — колонки CSV для событий (§6 спеки), в порядке, ожидаемом
// писателем. Stacktrace, contexts, breadcrumbs, request сюда не входят —
// в табличном виде они превращают выгрузку в нечитаемое полотно; в Record
// они есть всегда, но CSV-писатель берёт значения только по этому списку
// (см. writer.go: csvWriter.Write индексируется по columns, а не по Record
// целиком), так что для JSON/NDJSON те же Record приходят с полными полями.
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

// NewEventSource создаёт источник событий. includePII заявки Stream берёт
// параметром при каждом вызове (не полем конструктора): заявка — не
// свойство источника, а снимок галки КОНКРЕТНОЙ заявки («выгрузить как
// есть», доступна только админу/владельцу орга — проверяется на постановке
// заявки веб-слоем, не здесь), и один и тот же источник, живущий в Worker
// весь процесс (worker.go: Worker.Events), обслуживает заявки с разным
// значением галки одну за другой.
func NewEventSource(q *event.Query, issues *issue.Service) EventSource {
	return &eventSource{q: q, issues: issues, maxIssueIDs: defaultMaxIssueIDsForEventExport}
}

// Stream резолвит область выгрузки в список issue_id (resolveIssueIDs:
// IDsForFilter отдаёт его отсортированным по last_seen DESC, id DESC — самые
// активные группы первыми) и стримит события дальше как Record. Порядок
// строк — по StreamForExport: группами в порядке ЭТОГО списка, внутри
// группы timestamp DESC (K4-2, аудит перед 1.0) — так усечение по
// eventStreamSafetyLimit/бюджету заявки отбрасывает наименее активные
// группы, а не произвольные (раньше StreamForExport сортировал по
// возрастанию issue_id, не связанному с активностью группы).
func (s *eventSource) Stream(ctx context.Context, projectID, scopeIssueID int64, includePII bool, p Params, fn func(Record) error) error {
	issueIDs, err := s.resolveIssueIDs(ctx, projectID, scopeIssueID, p)
	if err != nil {
		return err
	}
	if len(issueIDs) == 0 {
		return nil
	}
	// Salt на ЭТОТ вызов Stream, то есть ровно на одну заявку: PseudonymizeUserID
	// в toRecord (см. её докблок в pii.go) даёт «одинаковый user_id — одинаковый
	// псевдоним» ВНУТРИ файла и разный псевдоним между разными выгрузками —
	// свойство держится только если salt не переживает один вызов Stream.
	salt := NewExportSalt()
	return s.q.StreamForExport(ctx, projectID, issueIDs, p.Since, p.Until, eventStreamSafetyLimit, func(ev event.Stored) error {
		return fn(s.toRecord(ev, includePII, salt))
	})
}

// resolveIssueIDs — «одна группа» (ScopeIssueID заявки задан) идёт прямо в
// CH-фильтр без похода в PG: заявка уже указывает конкретный id. «Проект с
// фильтрами» — id резолвятся из PG тем же Filter, что видел пользователь на
// экране списка issues (buildIssueFilter, общий с issue.List/StreamForExport).
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

// toRecord превращает событие CH в Record. При includePII == false прямые
// идентификаторы пользователя, свободный текст, стектрейс/breadcrumbs,
// запрос/контексты и теги маскируются тем же денилистом/скрабером, что и
// приём (MaskUser/PseudonymizeUserID/MaskMessage/MaskJSON/MaskTags,
// internal/export/pii.go):
//   - user_ip/user_email — статичная маска [masked] (MaskUser);
//   - user_id — стабильный внутри ЭТОЙ выгрузки псевдоним, не статичная
//     маска (PseudonymizeUserID; salt — на весь Stream, см. выше), иначе
//     терялась бы возможность посчитать «сколько пользователей задело»;
//   - message/exception_value — свободный текст, тот же скраб, что на
//     приёме (MaskMessage);
//   - stacktrace/breadcrumbs — ОБЯЗАНЫ идти через MaskJSON, как
//     request/contexts: breadcrumbs штатно несут URL с query-параметрами и
//     http-крошки, stacktrace — локальные переменные фрейма (frame-vars,
//     фича продукта). Раньше эти два поля маску не проходили вовсе — в
//     выгрузке «без ПДн» ПДн уезжали целиком именно через них;
//   - tags — email/IP, пришедший тегом (user.email, user_ip и т.п.), иначе
//     утекал бы мимо маски отдельных полей UserIP/UserEmail.
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

// flattenTags сериализует теги события в плоскую строку "k=v; k=v" для CSV
// (§6 спеки). Ключи отсортированы: порядок обхода map случаен, а без
// сортировки один и тот же набор тегов давал бы разные строки от запуска к
// запуску.
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

// rawJSON оборачивает JSON-текст события в json.RawMessage для писателя.
// Пустая строка — это «поле не пришло», а не «пришёл пустой JSON»: null в
// выгрузке отличим от {}, который событие могло прислать явно.
func rawJSON(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}
