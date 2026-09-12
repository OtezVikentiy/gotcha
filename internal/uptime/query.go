package uptime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

type CheckRow struct {
	Timestamp  time.Time
	Region     string
	OK         bool
	StatusCode uint16
	Error      string

	TotalMs, DNSMs, ConnectMs, TLSMs, TTFBMs uint32
}

// Avg* — среднее по проверкам, попавшим в интервал; 0, если проверок не было.
type LatencyPoint struct {
	T time.Time

	AvgTotalMs, AvgDNSMs, AvgConnectMs, AvgTLSMs, AvgTTFBMs uint32
}

type UptimeStat struct {
	Total, OK uint64
}

// 0, если проверок не было (Total == 0), а не деление на ноль.
func (s UptimeStat) Ratio() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.OK) / float64(s.Total)
}

// [From, To) — полуоткрытый промежуток, обычно окно обслуживания.
type Interval struct {
	From, To time.Time
}

// отдельный тип, не общий с trace или slo.Bucket: uptime не должен
// импортировать ни slo, ни trace — конвертацию делает провайдер.
type CountBucket struct {
	T           time.Time
	Good, Total uint64
}

// окна обслуживания здесь НЕ исключаются — это делает провайдер (по центру
// корзины), единообразно со всеми SLI.
func (q *Query) UpBuckets(ctx context.Context, monitorID int64, from, to time.Time, step time.Duration) ([]CountBucket, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("uptime: up buckets: step must be at least one second, got %s", step)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS t,
			sum(ok) AS good, count() AS total
		FROM check_results
		WHERE monitor_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY t
		ORDER BY t`,
		stepSec, uint64(monitorID), from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: up buckets: %w", err)
	}
	defer rows.Close()

	var out []CountBucket
	for rows.Next() {
		var t time.Time
		var good, total uint64
		if err := rows.Scan(&t, &good, &total); err != nil {
			return nil, fmt.Errorf("uptime: up buckets: scan: %w", err)
		}
		out = append(out, CountBucket{T: t.UTC(), Good: good, Total: total})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: up buckets: %w", err)
	}
	return out, nil
}

func (q *Query) Recent(ctx context.Context, monitorID int64, limit int) ([]CheckRow, error) {
	if limit <= 0 {
		return nil, nil
	}

	rows, err := q.conn.Query(ctx, `
		SELECT timestamp, region, ok, status_code, error, total_ms, dns_ms, connect_ms, tls_ms, ttfb_ms
		FROM check_results
		WHERE monitor_id = ?
		ORDER BY timestamp DESC
		LIMIT ?`,
		uint64(monitorID), limit)
	if err != nil {
		return nil, fmt.Errorf("uptime: recent: %w", err)
	}
	defer rows.Close()

	out := make([]CheckRow, 0, limit)
	for rows.Next() {
		var r CheckRow
		var ok uint8
		if err := rows.Scan(
			&r.Timestamp, &r.Region, &ok, &r.StatusCode, &r.Error,
			&r.TotalMs, &r.DNSMs, &r.ConnectMs, &r.TLSMs, &r.TTFBMs,
		); err != nil {
			return nil, fmt.Errorf("uptime: recent: scan: %w", err)
		}
		r.OK = ok != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: recent: %w", err)
	}
	return out, nil
}

// сетка выровнена по Unix epoch (toStartOfInterval) — time.Truncate здесь не
// годится, она должна совпадать с той, что строит ClickHouse.
func (q *Query) Latency(ctx context.Context, monitorID int64, from, to time.Time, step time.Duration) ([]LatencyPoint, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("uptime: latency: step must be at least one second, got %s", step)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS bucket_ts,
			toUInt32(round(avg(total_ms))), toUInt32(round(avg(dns_ms))),
			toUInt32(round(avg(connect_ms))), toUInt32(round(avg(tls_ms))), toUInt32(round(avg(ttfb_ms)))
		FROM check_results
		WHERE monitor_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY bucket_ts
		ORDER BY bucket_ts`,
		stepSec, uint64(monitorID), from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: latency: %w", err)
	}
	defer rows.Close()

	byBucket := make(map[int64]LatencyPoint)
	for rows.Next() {
		var t time.Time
		var p LatencyPoint
		if err := rows.Scan(&t, &p.AvgTotalMs, &p.AvgDNSMs, &p.AvgConnectMs, &p.AvgTLSMs, &p.AvgTTFBMs); err != nil {
			return nil, fmt.Errorf("uptime: latency: scan: %w", err)
		}
		byBucket[t.UTC().Unix()] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: latency: %w", err)
	}

	// Align grid to Unix epoch like ClickHouse toStartOfInterval does.
	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	// последняя корзина — та, что СОДЕРЖИТ момент to, не следующая за ним:
	// запрос фильтрует ts < to, так что в следующей не может быть данных.
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	var out []LatencyPoint
	for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
		cursor := time.Unix(curUnix, 0).UTC()
		p := byBucket[curUnix] // zero LatencyPoint if bucket has no checks
		p.T = cursor
		out = append(out, p)
	}
	return out, nil
}

// условие исключения собирается из параметров запроса — значения никогда не
// конкатенируются в текст запроса.
func (q *Query) Uptime(ctx context.Context, monitorID int64, from, to time.Time, exclude []Interval) (UptimeStat, error) {
	query := `SELECT count(), sum(ok) FROM check_results WHERE monitor_id = ? AND timestamp >= ? AND timestamp < ?`
	args := []any{uint64(monitorID), from, to}
	for _, iv := range exclude {
		query += ` AND NOT (timestamp >= ? AND timestamp < ?)`
		args = append(args, iv.From, iv.To)
	}

	row := q.conn.QueryRow(ctx, query, args...)
	var total, ok uint64
	if err := row.Scan(&total, &ok); err != nil {
		// агрегат без GROUP BY обычно отдаёт строку нулей, но при
		// empty_result_for_aggregation_by_empty_set=1 вернёт ErrNoRows.
		if errors.Is(err, sql.ErrNoRows) {
			return UptimeStat{}, nil
		}
		return UptimeStat{}, fmt.Errorf("uptime: uptime: %w", err)
	}
	return UptimeStat{Total: total, OK: ok}, nil
}

// в отличие от Uptime, окна обслуживания не учитываются — это «сырой» аптайм,
// список показывает как есть. Мониторы без проверок — UptimeStat{0, 0}.
func (q *Query) UptimeBatch(ctx context.Context, monitorIDs []int64, from, to time.Time) (map[int64]UptimeStat, error) {
	out := make(map[int64]UptimeStat, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = UptimeStat{}
	}

	ids := make([]uint64, len(monitorIDs))
	for i, id := range monitorIDs {
		ids[i] = uint64(id)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT monitor_id, count(), sum(ok)
		FROM check_results
		WHERE monitor_id IN (?) AND timestamp >= ? AND timestamp < ?
		GROUP BY monitor_id`,
		ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: uptime batch: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uint64
		var total, ok uint64
		if err := rows.Scan(&id, &total, &ok); err != nil {
			return nil, fmt.Errorf("uptime: uptime batch: scan: %w", err)
		}
		out[int64(id)] = UptimeStat{Total: total, OK: ok}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: uptime batch: %w", err)
	}
	return out, nil
}

// в отличие от UptimeBatch, здесь exclude учитывается — без него 90-дневный
// аптайм на публичной странице занижался бы плановым обслуживанием.
func (q *Query) UptimeExcludingBatch(ctx context.Context, monitorIDs []int64, from, to time.Time, exclude []Interval) (map[int64]UptimeStat, error) {
	out := make(map[int64]UptimeStat, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = UptimeStat{}
	}
	ids := make([]uint64, len(monitorIDs))
	for i, id := range monitorIDs {
		ids[i] = uint64(id)
	}

	query := `SELECT monitor_id, count(), sum(ok) FROM check_results WHERE monitor_id IN (?) AND timestamp >= ? AND timestamp < ?`
	args := []any{ids, from, to}
	for _, iv := range exclude {
		query += ` AND NOT (timestamp >= ? AND timestamp < ?)`
		args = append(args, iv.From, iv.To)
	}
	query += ` GROUP BY monitor_id`

	rows, err := q.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("uptime: uptime excluding batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		var total, ok uint64
		if err := rows.Scan(&id, &total, &ok); err != nil {
			return nil, fmt.Errorf("uptime: uptime excluding batch: scan: %w", err)
		}
		out[int64(id)] = UptimeStat{Total: total, OK: ok}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: uptime excluding batch: %w", err)
	}
	return out, nil
}

func (q *Query) LatencyBatch(ctx context.Context, monitorIDs []int64, from, to time.Time, step time.Duration) (map[int64][]LatencyPoint, error) {
	out := make(map[int64][]LatencyPoint, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("uptime: latency batch: step must be at least one second, got %s", step)
	}
	ids := make([]uint64, len(monitorIDs))
	for i, id := range monitorIDs {
		ids[i] = uint64(id)
	}

	perMon := make(map[int64]map[int64]LatencyPoint, len(monitorIDs))
	rows, err := q.conn.Query(ctx, `
		SELECT monitor_id, toStartOfInterval(timestamp, INTERVAL ? second) AS bucket_ts,
			toUInt32(round(avg(total_ms))), toUInt32(round(avg(dns_ms))),
			toUInt32(round(avg(connect_ms))), toUInt32(round(avg(tls_ms))), toUInt32(round(avg(ttfb_ms)))
		FROM check_results
		WHERE monitor_id IN (?) AND timestamp >= ? AND timestamp < ?
		GROUP BY monitor_id, bucket_ts
		ORDER BY bucket_ts`,
		stepSec, ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: latency batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid uint64
		var t time.Time
		var p LatencyPoint
		if err := rows.Scan(&mid, &t, &p.AvgTotalMs, &p.AvgDNSMs, &p.AvgConnectMs, &p.AvgTLSMs, &p.AvgTTFBMs); err != nil {
			return nil, fmt.Errorf("uptime: latency batch: scan: %w", err)
		}
		m := perMon[int64(mid)]
		if m == nil {
			m = make(map[int64]LatencyPoint)
			perMon[int64(mid)] = m
		}
		m[t.UTC().Unix()] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: latency batch: %w", err)
	}

	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	// последняя корзина — та, что СОДЕРЖИТ момент to, не следующая за ним:
	// запрос фильтрует ts < to, так что в следующей не может быть данных.
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}
	for _, id := range monitorIDs {
		byBucket := perMon[id]
		var pts []LatencyPoint
		for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
			p := byBucket[curUnix] // нулевой LatencyPoint, если в бакете проверок нет
			p.T = time.Unix(curUnix, 0).UTC()
			pts = append(pts, p)
		}
		out[id] = pts
	}
	return out, nil
}

func (q *Query) BarsBatch(ctx context.Context, monitorIDs []int64, from, to time.Time, buckets int) (map[int64][]UptimeStat, error) {
	out := make(map[int64][]UptimeStat, len(monitorIDs))
	// отсекаем вырожденный вход ДО make([]UptimeStat, buckets) — отрицательный
	// buckets иначе паникует в makeslice.
	if len(monitorIDs) == 0 || buckets <= 0 || !to.After(from) {
		for _, id := range monitorIDs {
			out[id] = nil
		}
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = make([]UptimeStat, buckets)
	}

	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	bucketSec := (toUnix - fromUnix) / int64(buckets)
	if bucketSec <= 0 {
		bucketSec = 1
	}
	ids := make([]uint64, len(monitorIDs))
	for i, id := range monitorIDs {
		ids[i] = uint64(id)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT monitor_id, toUInt32(floor((toUnixTimestamp(timestamp) - ?) / ?)) AS bucket, count(), sum(ok)
		FROM check_results
		WHERE monitor_id IN (?) AND timestamp >= ? AND timestamp < ?
		GROUP BY monitor_id, bucket`,
		fromUnix, bucketSec, ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: bars batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid uint64
		var bucket uint32
		var total, ok uint64
		if err := rows.Scan(&mid, &bucket, &total, &ok); err != nil {
			return nil, fmt.Errorf("uptime: bars batch: scan: %w", err)
		}
		bars := out[int64(mid)]
		if bars == nil {
			continue // строка для монитора не из запрошенного набора — пропускаем
		}
		idx := int(bucket)
		if idx >= buckets {
			idx = buckets - 1
		}
		bars[idx].Total += total
		bars[idx].OK += ok
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: bars batch: %w", err)
	}
	return out, nil
}

// продовые вызовы вытеснены BarsBatch — Bars остаётся эталонной реализацией
// арифметики корзин для теста паритета, не удалять.
func (q *Query) Bars(ctx context.Context, monitorID int64, from, to time.Time, buckets int) ([]UptimeStat, error) {
	if buckets <= 0 || !to.After(from) {
		return nil, nil
	}
	out := make([]UptimeStat, buckets)

	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	bucketSec := (toUnix - fromUnix) / int64(buckets)
	if bucketSec <= 0 {
		bucketSec = 1
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toUInt32(floor((toUnixTimestamp(timestamp) - ?) / ?)) AS bucket, count(), sum(ok)
		FROM check_results
		WHERE monitor_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY bucket`,
		fromUnix, bucketSec, uint64(monitorID), from, to)
	if err != nil {
		return nil, fmt.Errorf("uptime: bars: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucket uint32
		var total, ok uint64
		if err := rows.Scan(&bucket, &total, &ok); err != nil {
			return nil, fmt.Errorf("uptime: bars: scan: %w", err)
		}
		idx := int(bucket)
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].Total += total
		out[idx].OK += ok
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: bars: %w", err)
	}
	return out, nil
}
