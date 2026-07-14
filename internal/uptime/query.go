package uptime

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query — чтение агрегатов check_results из ClickHouse.
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// CheckRow — одна проверка, прочитанная как есть (для ленты "последние
// проверки").
type CheckRow struct {
	Timestamp  time.Time
	Region     string
	OK         bool
	StatusCode uint16
	Error      string

	TotalMs, DNSMs, ConnectMs, TLSMs, TTFBMs uint32
}

// LatencyPoint — точка временного ряда средней латентности: T — начало
// интервала (UTC), Avg* — средние по проверкам, попавшим в интервал (0, если
// проверок не было).
type LatencyPoint struct {
	T time.Time

	AvgTotalMs, AvgDNSMs, AvgConnectMs, AvgTLSMs, AvgTTFBMs uint32
}

// UptimeStat — доля успешных проверок за период.
type UptimeStat struct {
	Total, OK uint64
}

// Ratio возвращает долю успешных проверок (OK/Total); 0, если проверок не
// было (Total == 0), а не деление на ноль.
func (s UptimeStat) Ratio() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.OK) / float64(s.Total)
}

// Interval — полуоткрытый промежуток времени [From, To), обычно окно
// обслуживания, которое нужно исключить из расчёта аптайма.
type Interval struct {
	From, To time.Time
}

// Recent возвращает до limit последних проверок монитора, отсортированных по
// timestamp DESC (сначала самая новая).
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

// Latency строит временной ряд средней латентности монитора на окне
// [from, to) с шагом step: точки идут по шагу от from до to включительно
// (хронологически), пропуски (интервалы без проверок) заполняются нулями.
// Группировка выровнена по абсолютной сетке (toStartOfInterval),
// выровненной по Unix epoch — как в event.Query.Series (см.
// internal/event/query.go): time.Truncate здесь не годится, сетка должна
// совпадать с той, что строит ClickHouse.
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
	endUnix := (toUnix / stepSec) * stepSec
	if toUnix%stepSec > 0 {
		endUnix += stepSec
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

// Uptime считает долю успешных проверок монитора на окне [from, to),
// исключая проверки, попадающие в любой из intervals exclude (обычно окна
// обслуживания). Условие исключения собирается из параметров запроса
// (WHERE ... AND NOT (timestamp >= ? AND timestamp < ?) на каждый interval),
// значения никогда не конкатенируются в текст запроса. Пустой exclude — без
// дополнительного условия.
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
		return UptimeStat{}, fmt.Errorf("uptime: uptime: %w", err)
	}
	return UptimeStat{Total: total, OK: ok}, nil
}

// UptimeBatch считает долю успешных проверок на окне [from, to) для каждого
// из monitorIDs за один запрос — для списочного представления мониторов.
// В отличие от Uptime, окна обслуживания не учитываются: это «сырой» аптайм
// за период (например, за последние 24 часа), намеренно не скорректированный
// по exclude-интервалам — список показывает как есть. Мониторы без единой
// проверки в окне присутствуют в результате с UptimeStat{0, 0}.
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

// Bars разбивает [from, to) на buckets равных корзин и считает в каждой
// UptimeStat — для полоски доступности (например, 24 корзины за последние
// 24 часа, 90 — за 90 дней). Корзины без проверок остаются структурно
// нулевыми (UptimeStat{0, 0}), а не отсутствуют в слайсе: длина результата
// всегда равна buckets.
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
