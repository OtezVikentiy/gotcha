package event

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// Stored — событие, прочитанное из CH; JSON-поля (Stacktrace, Contexts)
// возвращаются как есть, без разбора. Stacktrace хранит весь JSON исключения
// вида {"values":[...]}.
type Stored struct {
	ID        string
	Timestamp time.Time
	Level     string
	Message   string

	ExceptionType  string
	ExceptionValue string
	Stacktrace     string

	Environment string
	Release     string
	ServerName  string
	SDK         string

	UserID    string
	UserIP    string
	UserEmail string

	Tags        map[string]string
	Contexts    string
	Breadcrumbs string
	Request     string // JSON: Sentry request-интерфейс (method/url/query_string/data/headers)

	// TraceID — trace_id события (пустой, если SDK трейсинг не включил):
	// страница issue показывает по нему ссылку «Смотреть трейс» на waterfall
	// (этап 3, план 4, задача 3).
	TraceID string
}

// Point — точка временного ряда: T — начало интервала (UTC), N — число
// событий, попавших в этот интервал.
type Point struct {
	T time.Time
	N uint64
}

// Query — чтение событий из ClickHouse.
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

const storedColumns = `event_id, timestamp, level, message, exception_type, exception_value, stacktrace,
	environment, release, server_name, sdk, user_id, user_ip, user_email, tags, contexts, trace_id, breadcrumbs, request`

// scanner — общая часть driver.Row и driver.Rows, достаточная для Scan.
type scanner interface {
	Scan(dest ...any) error
}

func scanStored(s scanner) (Stored, error) {
	var out Stored
	var id uuid.UUID
	if err := s.Scan(
		&id, &out.Timestamp, &out.Level, &out.Message,
		&out.ExceptionType, &out.ExceptionValue, &out.Stacktrace,
		&out.Environment, &out.Release, &out.ServerName, &out.SDK,
		&out.UserID, &out.UserIP, &out.UserEmail,
		&out.Tags, &out.Contexts, &out.TraceID, &out.Breadcrumbs, &out.Request,
	); err != nil {
		return Stored{}, err
	}
	out.ID = id.String()
	return out, nil
}

// EventsForIssue возвращает до limit последних событий issue, отсортированных
// по timestamp DESC (сначала самые новые).
func (q *Query) EventsForIssue(ctx context.Context, projectID, issueID int64, limit int) ([]Stored, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT `+storedColumns+`
		FROM events
		WHERE project_id = ? AND issue_id = ?
		ORDER BY timestamp DESC
		LIMIT ?`,
		uint64(projectID), uint64(issueID), limit)
	if err != nil {
		return nil, fmt.Errorf("query events for issue: %w", err)
	}
	defer rows.Close()

	out := make([]Stored, 0, limit)
	for rows.Next() {
		s, err := scanStored(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query events for issue: %w", err)
	}
	return out, nil
}

// CountSince возвращает число событий issue с timestamp >= since. Используется
// spike-воркером алертинга для сравнения с порогом правила.
func (q *Query) CountSince(ctx context.Context, projectID, issueID int64, since time.Time) (uint64, error) {
	row := q.conn.QueryRow(ctx, `
		SELECT count() FROM events
		WHERE project_id = ? AND issue_id = ? AND timestamp >= ?`,
		uint64(projectID), uint64(issueID), since)
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count since: %w", err)
	}
	return n, nil
}

// spikeCandidateLimit — потолок числа групп, возвращаемых CountsSince.
//
// Групп столько же, сколько issue превысили порог; в норме это единицы. Потолок
// нужен на случай общего отказа приложения, когда порог перешагнули тысячи
// групп: тогда лучше оповестить о самых громких, чем тащить в память список,
// длина которого управляется отправителем событий.
const spikeCandidateLimit = 500

// CountsSince возвращает число событий с момента since по всем группам проекта,
// у которых оно не меньше minCount.
//
// Один запрос вместо запроса на группу. Раньше spike-детектор звал CountSince в
// цикле по всем активным группам: 10 тысяч активных групп давали 10 тысяч
// round-trip'ов в ClickHouse каждую минуту с одной реплики — а число групп
// задаёт отправитель событий, через уникальный fingerprint.
//
// Порог применяется в HAVING, а не в Go: не превысившие его группы не нужны
// вовсе, и тащить их через сеть незачем.
func (q *Query) CountsSince(ctx context.Context, projectID int64, since time.Time, minCount uint64) (map[int64]uint64, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT issue_id, count() AS c FROM events
		WHERE project_id = ? AND timestamp >= ?
		GROUP BY issue_id
		HAVING c >= ?
		ORDER BY c DESC
		LIMIT ?`,
		uint64(projectID), since, minCount, spikeCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("counts since: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]uint64)
	for rows.Next() {
		var issueID, count uint64
		if err := rows.Scan(&issueID, &count); err != nil {
			return nil, fmt.Errorf("counts since scan: %w", err)
		}
		out[int64(issueID)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counts since: %w", err)
	}
	return out, nil
}

// EventByID ищет одно событие по project_id и event_id (UUID). Возвращает
// found=false, если события с таким id нет в проекте (включая случай, когда
// id существует, но принадлежит другому project_id).
func (q *Query) EventByID(ctx context.Context, projectID int64, eventID string) (Stored, bool, error) {
	id, err := uuid.Parse(eventID)
	if err != nil {
		return Stored{}, false, fmt.Errorf("parse event id: %w", err)
	}

	row := q.conn.QueryRow(ctx, `
		SELECT `+storedColumns+`
		FROM events
		WHERE project_id = ? AND event_id = ?
		LIMIT 1`,
		uint64(projectID), id)

	s, err := scanStored(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Stored{}, false, nil
		}
		return Stored{}, false, fmt.Errorf("scan event: %w", err)
	}
	return s, true, nil
}

// TraceError — событие-ошибка, привязанное к трейсу: issue_id, в котором оно
// сгруппировано, и span_id, на котором произошло (пустой — событие без привязки
// к конкретному спану). Для красных маркеров ошибок на waterfall трейса.
type TraceError struct {
	IssueID int64
	SpanID  string
}

// ByTraceID возвращает ошибки (события) проекта с данным trace_id — issue_id и
// span_id каждой, чтобы waterfall трейса пометил соответствующие спаны красным
// со ссылкой на issue. Параметризованный запрос: значения только через ?.
// Дубли (несколько событий одного issue на одном спане) схлопываются DISTINCT.
func (q *Query) ByTraceID(ctx context.Context, projectID int64, traceID string) ([]TraceError, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT issue_id, span_id
		FROM events
		WHERE project_id = ? AND trace_id = ?`,
		uint64(projectID), traceID)
	if err != nil {
		return nil, fmt.Errorf("query events by trace: %w", err)
	}
	defer rows.Close()

	var out []TraceError
	for rows.Next() {
		var issueID uint64
		var spanID string
		if err := rows.Scan(&issueID, &spanID); err != nil {
			return nil, fmt.Errorf("scan trace error: %w", err)
		}
		out = append(out, TraceError{IssueID: int64(issueID), SpanID: spanID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query events by trace: %w", err)
	}
	return out, nil
}

// Series строит временной ряд числа событий issue на окне [from, to) с шагом
// step: точки идут по шагу от from до to включительно (хронологически),
// пропуски (интервалы без событий) заполняются нулями. Группировка на
// стороне CH выровнена по абсолютной сетке (toStartOfInterval), выравненной
// по Unix epoch. Сетка на клиентской стороне строится с тем же выравниванием,
// чтобы гарантировать совпадение интервалов. Итоговая точка может быть
// структурно нулевой, так как граница запроса — < to.
func (q *Query) Series(ctx context.Context, projectID, issueID int64, from, to time.Time, step time.Duration) ([]Point, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("series: step must be at least one second, got %s", step)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS bucket_ts, count() AS n
		FROM events
		WHERE project_id = ? AND issue_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY bucket_ts
		ORDER BY bucket_ts`,
		stepSec, uint64(projectID), uint64(issueID), from, to)
	if err != nil {
		return nil, fmt.Errorf("query series: %w", err)
	}
	defer rows.Close()

	counts := make(map[int64]uint64)
	for rows.Next() {
		var t time.Time
		var n uint64
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("scan series point: %w", err)
		}
		counts[t.UTC().Unix()] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query series: %w", err)
	}

	// Align grid to Unix epoch like ClickHouse toStartOfInterval does.
	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	// Последняя корзина — та, что СОДЕРЖИТ момент to, а не следующая за ним.
	// Раньше граница округлялась ВВЕРХ за to, а цикл шёл по `<=`, поэтому в ряд
	// добавлялась корзина, начинающаяся в to или позже: запрос фильтрует ts < to,
	// так что данных в ней не могло быть ни при каких условиях. Спарклайны в
	// списках (мониторы, эндпойнты) не проходят через fillSeries и потому
	// заканчивались принудительным падением в ноль.
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	var out []Point
	for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
		cursor := time.Unix(curUnix, 0).UTC()
		out = append(out, Point{T: cursor, N: counts[curUnix]})
	}
	return out, nil
}

// Sparklines строит по каждому issueID равномерную гистограмму числа событий
// от since до текущего момента, разбитую на buckets равных интервалов.
// Отсутствующие в issueIDs корзины/issue не встречаются в out — для каждого
// запрошенного issueID гарантируется слайс длины buckets, недостающие
// значения — нули.
func (q *Query) Sparklines(ctx context.Context, projectID int64, issueIDs []int64, since time.Time, buckets int) (map[int64][]uint64, error) {
	out := make(map[int64][]uint64, len(issueIDs))
	if len(issueIDs) == 0 || buckets <= 0 {
		return out, nil
	}
	for _, id := range issueIDs {
		out[id] = make([]uint64, buckets)
	}

	sinceUnix := since.UTC().Unix()
	width := time.Now().UTC().Unix() - sinceUnix
	if width <= 0 {
		width = int64(buckets)
	}
	bucketSec := width / int64(buckets)
	if bucketSec <= 0 {
		bucketSec = 1
	}

	ids := make([]uint64, len(issueIDs))
	for i, id := range issueIDs {
		ids[i] = uint64(id)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT issue_id, toUInt32(floor((toUnixTimestamp(timestamp) - ?) / ?)) AS bucket, count() AS n
		FROM events
		WHERE project_id = ? AND issue_id IN (?) AND timestamp >= ?
		GROUP BY issue_id, bucket`,
		sinceUnix, bucketSec, uint64(projectID), ids, since)
	if err != nil {
		return nil, fmt.Errorf("query sparklines: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var issueID uint64
		var bucket uint32
		var n uint64
		if err := rows.Scan(&issueID, &bucket, &n); err != nil {
			return nil, fmt.Errorf("scan sparkline bucket: %w", err)
		}
		bs, ok := out[int64(issueID)]
		if !ok {
			continue
		}
		idx := int(bucket)
		if idx >= buckets {
			idx = buckets - 1
		}
		bs[idx] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query sparklines: %w", err)
	}
	return out, nil
}
