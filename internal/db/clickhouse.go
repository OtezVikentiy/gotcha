package db

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Бюджет ОДНОГО окна писателя, не всего флаша: драйвер снимает дедлайн посреди флаша
// (prepareBatch: defer SetDeadline(zero)), поэтому окон два, а у trace.flush с двумя батчами четыре.
// Самая длинная ветка остановки — 9 окон, и она обязана уложиться в stop_grace_period (90с).
const WriteBudget = 7 * time.Second

// Потолок runaway-запроса: тяжёлый скан иначе занимает сервер целиком. ReadTimeout здесь
// не занижаем — дефолтные 300с переживают длинный SELECT, а WriteBudget его бы оборвал.
func NewClickHouse(ctx context.Context, dsn string) (driver.Conn, error) {
	return openClickHouse(ctx, dsn, 60, 0, 0)
}

// Соединение писателей: ReadTimeout и max_execution_time воспроизводят WriteBudget впрямую; запись
// ограничивает wrapWriteDeadline — один дедлайн на непрерывную серию Write, а не на каждый вызов.
func NewClickHouseWriter(ctx context.Context, dsn string) (driver.Conn, error) {
	return openClickHouse(ctx, dsn, int((WriteBudget+5*time.Second)/time.Second), WriteBudget, WriteBudget)
}

func openClickHouse(ctx context.Context, dsn string, defaultMaxExecutionTime int, readTimeout, writeBudget time.Duration) (driver.Conn, error) {
	opts, err := buildOptions(dsn, defaultMaxExecutionTime, readTimeout, writeBudget)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	return conn, nil
}

func buildOptions(dsn string, defaultMaxExecutionTime int, readTimeout, writeBudget time.Duration) (*clickhouse.Options, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		// Сырую ошибку ParseDSN намеренно не оборачиваем и не логируем — она может содержать DSN с паролем.
		return nil, fmt.Errorf("clickhouse: invalid DSN")
	}
	// DSN (?max_execution_time=...) переопределяет — своё значение не трогаем.
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	if _, ok := opts.Settings["max_execution_time"]; !ok {
		opts.Settings["max_execution_time"] = defaultMaxExecutionTime
	}
	// Инвариант «агрегат без GROUP BY даёт одну строку с нулями» используют ~11 read-путей — фиксируем 0
	// жёстко, не даём DSN переопределить (иначе пустое окно даёт ErrNoRows вместо нулей).
	opts.Settings["empty_result_for_aggregation_by_empty_set"] = 0
	if readTimeout > 0 {
		// Безусловно, без уважения к ?read_timeout= из DSN: DSN один на оба соединения, и таймаут,
		// поднятый ради медленного SELECT, не должен растягивать единственный бюджет писателя.
		opts.ReadTimeout = readTimeout
	}
	if writeBudget > 0 {
		opts.DialContext = writeDeadlineDialer(opts.TLS, opts.DialTimeout, writeBudget)
	}
	return opts, nil
}

// Реплицирует дефолтный дозвон драйвера (conn.go:32-39: TLS-ветка при непустом tlsCfg, иначе
// net.DialTimeout, дефолт 30с при нулевом dialTimeout) — Send() сокетный дедлайн сам не ставит,
// поэтому дозвон отдаёт обёрнутое соединение (см. wrapWriteDeadline).
func writeDeadlineDialer(tlsCfg *tls.Config, dialTimeout, writeBudget time.Duration) func(ctx context.Context, addr string) (net.Conn, error) {
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}
	return func(_ context.Context, addr string) (net.Conn, error) {
		var (
			conn net.Conn
			err  error
		)
		if tlsCfg != nil {
			conn, err = tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, tlsCfg)
		} else {
			conn, err = net.DialTimeout("tcp", addr, dialTimeout)
		}
		if err != nil {
			return nil, err
		}
		return wrapWriteDeadline(conn, writeBudget), nil
	}
}

// connCheck (conn_check.go) снимает *tls.Conn через NetConn() и требует syscall.Conn у результата —
// разворачиваем TLS здесь же, на дозвоне, и делегируем SyscallConn() только когда raw-сокет реально
// есть: безусловный метод сделал бы плохим и net.Pipe() в тестах, чей SyscallConn() всегда с ошибкой.
func wrapWriteDeadline(conn net.Conn, budget time.Duration) net.Conn {
	base := &writeDeadlineConn{Conn: conn, budget: budget}
	raw := net.Conn(conn)
	if tlsConn, ok := conn.(*tls.Conn); ok {
		raw = tlsConn.NetConn()
	}
	if sc, ok := raw.(syscall.Conn); ok {
		return &writeDeadlineSyscallConn{writeDeadlineConn: base, raw: sc}
	}
	return base
}

type writeDeadlineConn struct {
	net.Conn
	budget time.Duration
	// Unix-наносекунды, 0 — не взведён. Переставляется только когда истёк: иначе флаш из нескольких
	// Write получал бы бюджет кратно. Атомик — инвариант «одна горутина» принадлежит драйверу, не нам.
	deadline atomic.Int64
}

// Драйвер снимает свой дедлайн между записями флаша (prepareBatch: defer SetDeadline(zero)) — его
// вызовы обязаны сбрасывать наш учёт, иначе выгрузка блока в Send() остаётся без потолка.
func (c *writeDeadlineConn) SetDeadline(t time.Time) error {
	c.storeDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *writeDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.storeDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *writeDeadlineConn) storeDeadline(t time.Time) {
	if t.IsZero() {
		c.deadline.Store(0)
		return
	}
	c.deadline.Store(t.UnixNano())
}

func (c *writeDeadlineConn) Write(p []byte) (int, error) {
	now := time.Now()
	if armed := c.deadline.Load(); armed == 0 || now.UnixNano() >= armed {
		deadline := now.Add(c.budget)
		c.deadline.Store(deadline.UnixNano())
		if err := c.Conn.SetWriteDeadline(deadline); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

type writeDeadlineSyscallConn struct {
	*writeDeadlineConn
	raw syscall.Conn
}

func (c *writeDeadlineSyscallConn) SyscallConn() (syscall.RawConn, error) {
	return c.raw.SyscallConn()
}

// Deadline свой; Done/Err наследуются от WithoutCancel (nil) — сторож batch.Send() бьёт только
// по Done и остаётся недостижим, а дедлайн для SetDeadline/max_execution_time в драйвере жив.
type batchContext struct {
	context.Context
	deadline time.Time
}

func (c batchContext) Deadline() (time.Time, bool) { return c.deadline, true }

// Отменяемый контекст сюда отдавать нельзя: Send заводит горутину, которая на Done() рвёт сырой
// сокет мимо пула, а тот к этому моменту уже может принадлежать чужому запросу. Done() == nil
// делает её недостижимой; budget воспроизводит прежний потолок через Deadline (см. batchContext).
func BatchContext(parent context.Context, budget time.Duration) context.Context {
	return batchContext{Context: context.WithoutCancel(parent), deadline: time.Now().Add(budget)}
}

// Тот же парсер, что и NewClickHouse — принимает URL- и keyword-форму DSN одинаково.
func ValidateClickHouseDSN(dsn string) error {
	if _, err := clickhouse.ParseDSN(dsn); err != nil {
		// Как и выше — сырая ошибка может содержать DSN с паролем, поэтому не логируем её.
		return fmt.Errorf("clickhouse: invalid DSN")
	}
	return nil
}
