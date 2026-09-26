package db_test

import (
	"context"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Соединение, легшее в простой мгновение назад, дефолтный ShouldPing pgxpool
// не пингует (порог ≥1с) — следующий Acquire получает его мёртвым молча.
func TestPoolDropsDeadConnectionsOnAcquireEvenWhenRecentlyIdle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var x int
			errs <- pool.QueryRow(ctx, "select 1").Scan(&x)
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond) // простой меньше 1с — порог дефолтного ShouldPing не достигнут

	admin, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`select pg_terminate_backend(pid) from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid()`,
	); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	admin.Release()
	time.Sleep(200 * time.Millisecond) // суммарный простой всё ещё меньше 1с

	errs2 := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var x int
			errs2 <- pool.QueryRow(ctx, "select 1").Scan(&x)
		}()
	}
	var failed int
	for i := 0; i < n; i++ {
		if err := <-errs2; err != nil {
			failed++
			t.Logf("query on a connection killed by pg_terminate_backend: %v", err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d queries hit a dead pooled connection instead of being transparently replaced", failed, n)
	}
}

// proxyConn.closed фиксирует факт, а не время: выставляется, только когда
// сокет действительно закрылся — заморозка сама по себе его не трогает.
type proxyConn struct {
	frozen atomic.Bool
	closed atomic.Bool
}

// freezeProxy передаёт байты в обе стороны, пока не заморожен — после freezeAll
// уже установленные соединения перестают долетать до клиента, не закрываясь.
type freezeProxy struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	conns    []*proxyConn
	sockets  []net.Conn
}

func newFreezeProxy(t *testing.T, upstream string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln, upstream: upstream}
	go p.acceptLoop()
	return p
}

func (p *freezeProxy) acceptLoop() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		pc := &proxyConn{}
		p.mu.Lock()
		p.conns = append(p.conns, pc)
		p.sockets = append(p.sockets, client, upstream)
		p.mu.Unlock()

		// closed фиксируется только по СТОРОНЕ upstream: клиент (pgx) сам
		// закрывается после честного таймаута пинга — это не сигнал.
		go func() {
			buf := make([]byte, 4096)
			for {
				n, rerr := client.Read(buf)
				if n > 0 {
					if _, werr := upstream.Write(buf[:n]); werr != nil {
						pc.closed.Store(true)
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := upstream.Read(buf)
				if n > 0 && !pc.frozen.Load() {
					if _, werr := client.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					pc.closed.Store(true)
					return
				}
			}
		}()
	}
}

func (p *freezeProxy) freezeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.frozen.Store(true)
	}
}

// anyClosed — факт, не измерение: заглушка, которая зависает как задумано,
// не закрывает сокет сама, сколько бы под нагрузкой ни тянулось ожидание.
func (p *freezeProxy) anyClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		if c.frozen.Load() && c.closed.Load() {
			return true
		}
	}
	return false
}

// Пул согласует закрытие с недостижимым концом и виснет секундами — закрываем
// сокеты явно до pool.Close (Cleanup регистрируется после него: LIFO).
func (p *freezeProxy) closeAll() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.sockets {
		_ = c.Close()
	}
}

// Соединение зависает, не закрывается — ответа не будет никогда. Acquire
// обязан уложиться в PingTimeout, не в дедлайн ctx вызывающего (здесь его нет).
func TestAcquireBoundedByOwnPingTimeoutNotCallerContext(t *testing.T) {
	realDSN := testenv.PostgresDSN(t)
	u, err := url.Parse(realDSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFreezeProxy(t, u.Host)
	u.Host = proxy.ln.Addr().String()

	pool, err := db.NewPostgres(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(proxy.closeAll)

	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("warmup acquire: %v", err)
	}
	var x int
	if err := c.QueryRow(ctx, "select 1").Scan(&x); err != nil {
		t.Fatalf("warmup query: %v", err)
	}
	c.Release()

	proxy.freezeAll()

	// Ниже этого элапсед не мог дождаться PingTimeout=1с — заглушка закрыла
	// соединение вместо того чтобы зависнуть, тест проверял бы не тот сценарий.
	const lowerBound = 900 * time.Millisecond
	const upperBound = 5 * time.Second
	done := make(chan struct{})
	var acquireErr error
	var elapsed time.Duration
	go func() {
		start := time.Now()
		c2, err := pool.Acquire(context.Background()) // без дедлайна нарочно
		elapsed = time.Since(start)
		acquireErr = err
		if err == nil {
			c2.Release()
		}
		close(done)
	}()

	select {
	case <-done:
		if acquireErr != nil {
			t.Fatalf("acquire against a hung connection failed instead of replacing it: %v", acquireErr)
		}
		if proxy.anyClosed() {
			t.Fatalf("stub closed a connection instead of leaving it hanging — ping timeout was not exercised")
		}
		if elapsed < lowerBound {
			t.Fatalf("acquire against a hung connection took %v, under %v — stub closed instead of hanging", elapsed, lowerBound)
		}
		t.Logf("acquire against a hung connection took %v", elapsed)
	case <-time.After(upperBound):
		t.Fatalf("acquire did not return within %v against a hung connection — ping is not bounded by its own timeout", upperBound)
	}
}
