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

// JSON-поля (Stacktrace, Contexts) возвращаются как есть, без разбора.
// Stacktrace хранит весь JSON исключения вида {"values":[...]}.
type Stored struct {
	ID        string
	IssueID   int64 // группа события; нужна выгрузке событий (kind=events) для колонки issue_id
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

	// Пустой, если SDK трейсинг не включил — по нему строится ссылка «Смотреть трейс».
	TraceID string
}

// T — начало интервала в UTC, N — число событий в нём.
type Point struct {
	T time.Time
	N uint64
}

type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

const storedColumns = `event_id, issue_id, timestamp, level, message, exception_type, exception_value, stacktrace,
	environment, release, server_name, sdk, user_id, user_ip, user_email, tags, contexts, trace_id, breadcrumbs, request`

type scanner interface {
	Scan(dest ...any) error
}

func scanStored(s scanner) (Stored, error) {
	var out Stored
	var id uuid.UUID
	var issueID uint64
	if err := s.Scan(
		&id, &issueID, &out.Timestamp, &out.Level, &out.Message,
		&out.ExceptionType, &out.ExceptionValue, &out.Stacktrace,
		&out.Environment, &out.Release, &out.ServerName, &out.SDK,
		&out.UserID, &out.UserIP, &out.UserEmail,
		&out.Tags, &out.Contexts, &out.TraceID, &out.Breadcrumbs, &out.Request,
	); err != nil {
		return Stored{}, err
	}
	out.ID = id.String()
	out.IssueID = int64(issueID)
	return out, nil
}

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

// Потолок нужен на случай общего отказа приложения, когда порог перешагнули
// тысячи групп: лучше оповестить о самых громких, чем разбухать памятью.
const spikeCandidateLimit = 500

// Один запрос вместо запроса на группу — иначе N round-trip'ов в CH на N активных групп.
// Порог — в HAVING, не в Go: не превысившие его группы через сеть не тащим.
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

// found=false — событий с таким id нет в проекте, в т.ч. если id принадлежит другому project_id.
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

// SpanID пустой — событие без привязки к конкретному спану.
type TraceError struct {
	IssueID int64
	SpanID  string
}

// DISTINCT схлопывает дубли — несколько событий одного issue на одном спане.
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

// Сетка выровнена по Unix epoch — как toStartOfInterval в CH — чтобы совпадать с группировкой.
// Итоговая точка может быть структурно нулевой: граница запроса — < to.
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
	// Последняя корзина — та, что содержит to, а не следующая: запрос фильтрует
	// ts < to, следующая корзина гарантированно пуста.
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

// Для каждого issueID в out — слайс длины buckets, отсутствующие значения — нули.
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

// Группа обходится одним запросом на issue_id, не общим IN(...)+ORDER BY — тот план
// читает и сортирует всё в памяти (MEMORY_LIMIT_EXCEEDED на большой таблице).
func (q *Query) StreamForExport(ctx context.Context, projectID int64, issueIDs []int64,
	since, until time.Time, limit int, fn func(Stored) error) error {
	remaining := limit
	for _, issueID := range issueIDs {
		if remaining <= 0 {
			break
		}

		query := `SELECT ` + storedColumns + `
			FROM events
			WHERE project_id = ? AND issue_id = ?`
		args := []any{uint64(projectID), uint64(issueID)}
		if !since.IsZero() {
			query += ` AND timestamp >= ?`
			args = append(args, since)
		}
		if !until.IsZero() {
			query += ` AND timestamp < ?`
			args = append(args, until)
		}
		query += ` ORDER BY timestamp DESC LIMIT ?`
		args = append(args, remaining)

		n, err := q.streamOneIssue(ctx, query, args, fn)
		if err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

// Вынесено отдельно, чтобы rows.Close() отрабатывал на каждой группе сразу —
// defer в цикле копил бы rows до возврата функции.
func (q *Query) streamOneIssue(ctx context.Context, query string, args []any, fn func(Stored) error) (int, error) {
	rows, err := q.conn.Query(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("stream for export: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		s, err := scanStored(rows)
		if err != nil {
			return n, fmt.Errorf("stream for export scan: %w", err)
		}
		if err := fn(s); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("stream for export: %w", err)
	}
	return n, nil
}
