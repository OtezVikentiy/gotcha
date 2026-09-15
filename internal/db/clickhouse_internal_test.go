package db

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func TestBatchContextDoneNilAfterParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := BatchContext(parent, 5*time.Second)
	cancel()

	if ctx.Done() != nil {
		t.Fatal("Done() != nil после отмены родителя — сторож batch.Send() снова достижим")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func TestBatchContextDeadline(t *testing.T) {
	const budget = 5 * time.Second
	ctx := BatchContext(context.Background(), budget)

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("Deadline() ok = false, want true")
	}
	if until := time.Until(deadline); until <= 0 || until > budget {
		t.Fatalf("time.Until(Deadline()) = %v, want (0, %v]", until, budget)
	}
}

func TestBatchContextValuePassesThroughParent(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "v")
	ctx := BatchContext(parent, time.Second)

	if got := ctx.Value(key{}); got != "v" {
		t.Fatalf("Value() = %v, want %q", got, "v")
	}
}

func TestBuildOptionsReaderVsWriter(t *testing.T) {
	const dsn = "clickhouse://gotcha:gotcha@localhost:9000/gotcha"

	readerOpts, err := buildOptions(dsn, 60, 0, 0)
	if err != nil {
		t.Fatalf("buildOptions reader: %v", err)
	}
	if got := readerOpts.Settings["max_execution_time"]; got != 60 {
		t.Errorf("reader max_execution_time = %v, want 60", got)
	}
	if readerOpts.ReadTimeout != 0 {
		t.Errorf("reader ReadTimeout = %v, want 0 (driver default 300s applies)", readerOpts.ReadTimeout)
	}
	if readerOpts.DialContext != nil {
		t.Error("reader DialContext != nil — таймаут записи должен ставиться только у писателя")
	}

	writerOpts, err := buildOptions(dsn, 15, WriteBudget, WriteBudget)
	if err != nil {
		t.Fatalf("buildOptions writer: %v", err)
	}
	if got := writerOpts.Settings["max_execution_time"]; got != 15 {
		t.Errorf("writer max_execution_time = %v, want 15", got)
	}
	if writerOpts.ReadTimeout != WriteBudget {
		t.Errorf("writer ReadTimeout = %v, want %v", writerOpts.ReadTimeout, WriteBudget)
	}
	if writerOpts.DialContext == nil {
		t.Error("writer DialContext == nil — Send() ничем не ограничен на записи")
	}
}

func TestBuildOptionsDSNOverrides(t *testing.T) {
	const dsn = "clickhouse://gotcha:gotcha@localhost:9000/gotcha?max_execution_time=42&read_timeout=33s"

	readerOpts, err := buildOptions(dsn, 60, 0, 0)
	if err != nil {
		t.Fatalf("buildOptions reader: %v", err)
	}
	if got := readerOpts.Settings["max_execution_time"]; got != 42 {
		t.Errorf("reader max_execution_time = %v, want DSN override 42", got)
	}
	if readerOpts.ReadTimeout != 33*time.Second {
		t.Errorf("reader ReadTimeout = %v, want DSN override 33s", readerOpts.ReadTimeout)
	}

	// Писательский ReadTimeout переопределяется безусловно — ?read_timeout= из DSN
	// не должен растягивать единственный бюджет писателя (см. buildOptions).
	writerOpts, err := buildOptions(dsn, 15, WriteBudget, WriteBudget)
	if err != nil {
		t.Fatalf("buildOptions writer: %v", err)
	}
	if got := writerOpts.Settings["max_execution_time"]; got != 42 {
		t.Errorf("writer max_execution_time = %v, want DSN override 42", got)
	}
	if writerOpts.ReadTimeout != WriteBudget {
		t.Errorf("writer ReadTimeout = %v, want unconditional %v despite DSN read_timeout=33s", writerOpts.ReadTimeout, WriteBudget)
	}
}

func TestWriteDeadlineDialerTimesOutOnWrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			defer conn.Close()
			<-accepted // держим соединение открытым и ничего не читаем
		}
	}()
	defer close(accepted)

	dial := writeDeadlineDialer(nil, 0, -time.Second) // дедлайн уже в прошлом
	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte("x"))
	if err == nil {
		t.Fatal("Write: want timeout error, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Write error = %v, want net.Error with Timeout() == true", err)
	}
}

// recordingConn — минимальный net.Conn, который считает вызовы Write и запоминает каждый
// SetWriteDeadline: нужен, чтобы проверить, что writeDeadlineConn делит один дедлайн на серию
// записей, не взводит новый на каждый Write.
type recordingConn struct {
	net.Conn
	writes              int
	writeDeadlines      []time.Time
	deadlines           []time.Time
	setWriteDeadlineErr error // если задан, SetWriteDeadline возвращает его вместо записи дедлайна
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.writes++
	return len(p), nil
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	if c.setWriteDeadlineErr != nil {
		return c.setWriteDeadlineErr
	}
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}

func (c *recordingConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}

func (c *recordingConn) Close() error { return nil }

// Симметрия к SetDeadline: перехват обязан быть на обоих сеттерах, иначе вызов SetWriteDeadline
// (в v2.47 драйвер его не зовёт, но это его внутренняя деталь) рассинхронизировал бы учёт серии.
func TestWriteDeadlineConnSetWriteDeadlineResetsArming(t *testing.T) {
	rc := &recordingConn{}
	c := &writeDeadlineConn{Conn: rc, budget: time.Minute}

	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write #1: %v", err)
	}
	if err := c.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline(zero): %v", err)
	}
	if _, err := c.Write([]byte("y")); err != nil {
		t.Fatalf("Write #2: %v", err)
	}

	if got := len(rc.writeDeadlines); got != 3 {
		t.Fatalf("SetWriteDeadline долетел до нижнего conn %d раз, want 3 (взвод, снятие, перевзвод)", got)
	}
	if rc.writeDeadlines[2].IsZero() {
		t.Fatal("после снятия дедлайна Write не перевзвёл его — серия осталась бы без потолка")
	}
}

// Драйвер снимает дедлайн сокета между записями одного флаша (prepareBatch: SetDeadline(D), запись
// запроса, defer SetDeadline(zero); затем Send пишет блок). Если обёртка этого не замечает, серия
// числится взведённой, потолка на сокете уже нет, и выгрузка блока идёт без ограничения.
func TestWriteDeadlineConnRearmsAfterDriverClearsDeadline(t *testing.T) {
	rc := &recordingConn{}
	c := &writeDeadlineConn{Conn: rc, budget: time.Minute}

	if _, err := c.Write([]byte("query")); err != nil {
		t.Fatalf("Write запроса: %v", err)
	}
	if got := len(rc.writeDeadlines); got != 1 {
		t.Fatalf("SetWriteDeadline вызван %d раз на первой записи, want 1", got)
	}

	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline(zero): %v", err)
	}

	if _, err := c.Write([]byte("block")); err != nil {
		t.Fatalf("Write блока: %v", err)
	}
	if got := len(rc.writeDeadlines); got != 2 {
		t.Fatalf("после SetDeadline(zero) драйвера Write не перевзвёл дедлайн (SetWriteDeadline вызван %d раз, want 2) — выгрузка блока в Send() осталась бы без потолка", got)
	}
}

func TestWriteDeadlineConnSharesDeadlineAcrossBurst(t *testing.T) {
	rc := &recordingConn{}
	const budget = 50 * time.Millisecond
	c := &writeDeadlineConn{Conn: rc, budget: budget}

	for i := 0; i < 3; i++ {
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if got := len(rc.writeDeadlines); got != 1 {
		t.Fatalf("SetWriteDeadline вызван %d раз за 3 Write подряд, want 1 — один дедлайн на серию, не на каждую запись", got)
	}

	time.Sleep(budget + 20*time.Millisecond)
	if _, err := c.Write([]byte("y")); err != nil {
		t.Fatalf("Write 4: %v", err)
	}
	if got := len(rc.writeDeadlines); got != 2 {
		t.Fatalf("SetWriteDeadline вызван %d раз после истечения бюджета, want 2 — новая серия должна получить новый дедлайн", got)
	}
	if !rc.writeDeadlines[1].After(rc.writeDeadlines[0]) {
		t.Fatalf("второй дедлайн %v не позже первого %v", rc.writeDeadlines[1], rc.writeDeadlines[0])
	}
	if rc.writes != 4 {
		t.Fatalf("Write долетел до нижнего conn %d раз, want 4", rc.writes)
	}
}

// probeConnCheck повторяет ровно то, что делает драйверный connCheck (conn_check.go): снимает
// syscall.Conn и читает один байт напрямую с fd. checked == false означает то же, что connCheck
// молча пропускает проверку живости (нет raw-сокета) — тест отличает это от реального EOF.
func probeConnCheck(conn net.Conn) (checked bool, err error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false, nil
	}
	rawConn, rcErr := sc.SyscallConn()
	if rcErr != nil {
		return true, rcErr
	}
	var sysErr error
	if rdErr := rawConn.Read(func(fd uintptr) bool {
		var buf [1]byte
		n, rerr := syscall.Read(int(fd), buf[:])
		switch {
		case n == 0 && rerr == nil:
			sysErr = io.EOF
		case n > 0:
			sysErr = errors.New("unexpected read from socket")
		case rerr == syscall.EAGAIN || rerr == syscall.EWOULDBLOCK:
			sysErr = nil
		default:
			sysErr = rerr
		}
		return true
	}); rdErr != nil {
		return true, rdErr
	}
	return true, sysErr
}

func waitForConnCheckEOF(t *testing.T, conn net.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		checked, err := probeConnCheck(conn)
		if !checked {
			t.Fatal("conn.(syscall.Conn) = false, want true — connCheck пропустит проверку живости")
		}
		if err == io.EOF {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connCheck-проба: want io.EOF, got %v после 5с ожидания", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWrapWriteDeadlinePreservesConnCheckOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			conn.Close() // закрываем серверную сторону сразу после дозвона
		}
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("Dial: %v", err)
	}
	ln.Close()
	conn := wrapWriteDeadline(raw, time.Second)
	defer conn.Close()

	waitForConnCheckEOF(t, conn)
}

func TestWrapWriteDeadlinePreservesConnCheckOverTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()

	raw, err := tls.Dial("tcp", ts.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	conn := wrapWriteDeadline(raw, time.Second)
	defer conn.Close()

	ts.CloseClientConnections() // закрывает серверную сторону этого TCP-сокета под TLS

	waitForConnCheckEOF(t, conn)
}

func TestWrapWriteDeadlineNoSyscallConnOverPipe(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	conn := wrapWriteDeadline(client, time.Second)
	defer conn.Close()

	checked, _ := probeConnCheck(conn)
	if checked {
		t.Fatal("обёртка над net.Pipe() реализует syscall.Conn — connCheck решит, что сокет плохой, хотя raw-сокета нет")
	}
}

// Ветка TLS самого writeDeadlineDialer — не wrapWriteDeadline напрямую, как выше: ошибка в
// ServerName/рукопожатии здесь сломала бы TLS-инсталляции молча (спека требует один-в-один
// повторить conn.go:32-39, включая эту ветку).
func TestWriteDeadlineDialerConnectsOverTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()

	dial := writeDeadlineDialer(&tls.Config{InsecureSkipVerify: true}, 0, time.Second)
	conn, err := dial(context.Background(), ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if checked, _ := probeConnCheck(conn); !checked {
		t.Fatal("conn.(syscall.Conn) = false, want true — TLS-ветка дозвона обязана давать ту же обёртку, что и обычный TCP")
	}
}

func TestWriteDeadlineDialerDialError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // порт свободен, но никто не слушает — соединение будет отвергнуто

	dial := writeDeadlineDialer(nil, time.Second, time.Second)
	if _, err := dial(context.Background(), addr); err == nil {
		t.Fatal("dial на закрытый порт: want error, got nil")
	}
}

func TestWriteDeadlineConnWriteReturnsSetDeadlineError(t *testing.T) {
	wantErr := errors.New("boom")
	rc := &recordingConn{setWriteDeadlineErr: wantErr}
	c := &writeDeadlineConn{Conn: rc, budget: time.Second}

	_, err := c.Write([]byte("x"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write err = %v, want %v", err, wantErr)
	}
	if rc.writes != 0 {
		t.Fatalf("Write долетел до нижнего conn при ошибке SetWriteDeadline, writes = %d, want 0", rc.writes)
	}
}
