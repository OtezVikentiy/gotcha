package uptime_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// checkerMonitor builds a bare Monitor (no DB) suitable for a pure Checker
// call — only Kind/TimeoutSeconds/Config matter to checkers.
func checkerMonitor(kind uptime.Kind, timeoutSeconds int, cfg json.RawMessage) uptime.Monitor {
	return uptime.Monitor{
		Kind:           kind,
		TimeoutSeconds: timeoutSeconds,
		Config:         cfg,
	}
}

// --- HTTP ---

func TestHTTPCheckerOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	if got.BodySize != uint32(len("hello world")) {
		t.Errorf("BodySize = %d, want %d", got.BodySize, len("hello world"))
	}
}

func TestHTTPChecker500WithDefaultExpectedStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
	if got.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if got.Error == "" {
		t.Errorf("Error is empty, want a message")
	}
}

func TestHTTPCheckerExpectedStatusMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:         "GET",
		URL:            srv.URL,
		ExpectedStatus: []int{404},
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK (404 is expected)", got)
	}
	if got.StatusCode != 404 {
		t.Errorf("StatusCode = %d, want 404", got.StatusCode)
	}
}

func TestHTTPCheckerBodyContainsFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("status: healthy"))
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:       "GET",
		URL:          srv.URL,
		BodyContains: "healthy",
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
}

func TestHTTPCheckerBodyContainsNotFoundFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("status: down"))
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:       "GET",
		URL:          srv.URL,
		BodyContains: "healthy",
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
	if got.Error == "" {
		t.Errorf("Error is empty, want a message")
	}
}

func TestHTTPCheckerBodyNotContainsFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("internal error occurred"))
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             srv.URL,
		BodyNotContains: "error",
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
}

func redirectServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("target"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPCheckerRedirectNotFollowedExpected301(t *testing.T) {
	srv := redirectServer(t)

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             srv.URL + "/redirect",
		FollowRedirects: false,
		ExpectedStatus:  []int{301},
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK (301 expected)", got)
	}
	if got.StatusCode != 301 {
		t.Errorf("StatusCode = %d, want 301", got.StatusCode)
	}
}

func TestHTTPCheckerRedirectNotFollowedUnexpectedFails(t *testing.T) {
	srv := redirectServer(t)

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             srv.URL + "/redirect",
		FollowRedirects: false,
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail (301 not in default 200..299)", got)
	}
	if got.StatusCode != 301 {
		t.Errorf("StatusCode = %d, want 301", got.StatusCode)
	}
}

func TestHTTPCheckerRedirectFollowedReachesTarget(t *testing.T) {
	srv := redirectServer(t)

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method:          "GET",
		URL:             srv.URL + "/redirect",
		FollowRedirects: true,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
}

func TestHTTPCheckerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 1, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail (timeout)", got)
	}
	if !strings.Contains(got.Error, "timeout") {
		t.Errorf("Error = %q, want it to mention timeout", got.Error)
	}
}

func TestHTTPCheckerTimingsNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK {
		t.Fatalf("Check() = %+v, want OK", got)
	}
	if got.TotalMs == 0 {
		t.Errorf("TotalMs = 0, want > 0")
	}
}

func TestHTTPCheckerTLSFillsSSLExpiresAt(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	certPool := x509.NewCertPool()
	certPool.AddCert(srv.Certificate())

	c := uptime.NewHTTPChecker(true)
	c.TLSClientConfig = &tls.Config{RootCAs: certPool}
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
	if got.SSLExpiresAt == nil {
		t.Fatalf("SSLExpiresAt is nil, want set")
	}
	if got.SSLExpiresAt.Before(time.Now()) {
		t.Errorf("SSLExpiresAt = %v, want in the future", got.SSLExpiresAt)
	}
	if got.TLSMs == 0 {
		t.Errorf("TLSMs = 0, want > 0")
	}
}

func TestHTTPCheckerBodyCappedAt1MB(t *testing.T) {
	const overLimit = (1 << 20) + 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, overLimit)
		for i := range buf {
			buf[i] = 'a'
		}
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK {
		t.Fatalf("Check() = %+v, want OK", got)
	}
	if got.BodySize != 1<<20 {
		t.Errorf("BodySize = %d, want capped at 1MB (%d)", got.BodySize, 1<<20)
	}
}

// TestHTTPCheckerBlocksLoopbackWhenPrivateDisallowed — при allowPrivate=false
// чекер режет соединение к loopback (SSRF-фильтр по умолчанию): результат down
// с ошибкой блокировки, до сервера запрос не доходит.
func TestHTTPCheckerBlocksLoopbackWhenPrivateDisallowed(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(false)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want down (loopback blocked)", got)
	}
	if !strings.Contains(got.Error, "blocked") {
		t.Errorf("Error = %q, want it to mention blocked target", got.Error)
	}
	if hit {
		t.Error("request reached the loopback server, want it blocked before dial")
	}
}

// TestHTTPCheckerAllowsLoopbackWhenPrivateAllowed — при allowPrivate=true
// фильтр отключён и запрос к loopback доходит.
func TestHTTPCheckerAllowsLoopbackWhenPrivateAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := uptime.NewHTTPChecker(true)
	m := checkerMonitor(uptime.KindHTTP, 5, httpConfig(t, uptime.HTTPConfig{
		Method: "GET",
		URL:    srv.URL,
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK (allowPrivate=true)", got)
	}
}

// --- TCP ---

// TestTCPCheckerBlocksLoopbackWhenPrivateDisallowed — при allowPrivate=false
// TCP-чекер режет коннект к loopback: результат down с ошибкой блокировки.
func TestTCPCheckerBlocksLoopbackWhenPrivateDisallowed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	c := uptime.NewTCPChecker(false)
	m := checkerMonitor(uptime.KindTCP, 5, tcpConfig(t, uptime.TCPConfig{Host: host, Port: port}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want down (loopback blocked)", got)
	}
	if !strings.Contains(got.Error, "blocked") {
		t.Errorf("Error = %q, want it to mention blocked target", got.Error)
	}
}

func TestTCPCheckerConnectsToLiveListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	c := uptime.NewTCPChecker(true)
	m := checkerMonitor(uptime.KindTCP, 5, tcpConfig(t, uptime.TCPConfig{Host: host, Port: port}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
}

func TestTCPCheckerFailsOnClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	ln.Close() // free the port so the connection is refused

	c := uptime.NewTCPChecker(true)
	m := checkerMonitor(uptime.KindTCP, 5, tcpConfig(t, uptime.TCPConfig{Host: host, Port: port}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
	if got.Error == "" {
		t.Errorf("Error is empty, want a message")
	}
}

// --- DNS ---

// fakeDNSHostname — заведомо несуществующее имя для fakeIPResolver.
//
// PreferGo=true форсирует чистый Go-резолвер, но для ЛЮБОГО имени (включая
// "localhost", которое здесь использовалось раньше) он сперва пробует
// путь "files", т.е. читает /etc/hosts, и только при промахе идёт в DNS.
// Запись "127.0.0.1 localhost" есть практически везде, так что с реальным
// "localhost" резолвер находил ответ в /etc/hosts и ни разу не обращался к
// нашему Dial — весь протокольный разбор ниже был мёртвым кодом, а тест был
// зелёным не по той причине (заглушка не проверяла ничего). Синтетическое
// имя с TLD .invalid не может встретиться в /etc/hosts (там нет и не может
// появиться такой записи), поэтому "files" гарантированно промахивается и
// путь идёт в DNS — то есть в наш Dial. TLD .invalid к тому же зарезервирован
// RFC 2606/6761 и никогда не делегируется в реальном DNS, так что даже если
// тест по ошибке всё-таки попадёт на системный резолвер (а не на fakeIPResolver),
// имя не разрешится случайно в чей-то настоящий адрес — тест просто упадёт
// явно, а не тихо проверит не то.
const fakeDNSHostname = "gotcha-fake-dns-test.invalid"

// fakeIPResolver возвращает *net.Resolver, отвечающий на A-запрос hostname
// заданным ip без обращения к системному резолверу и сети. Приём с подставным
// Dial уже применяется в check_dns_extra_test.go (там Dial сразу возвращает
// ошибку, чтобы детерминированно проверить путь отказа) — здесь тот же Dial
// отвечает по протоколу, чтобы детерминированно проверить путь успеха.
//
// Возвращаемый net.Pipe-конец не реализует net.PacketConn, так что резолвер
// сам выбирает потоковый framing (2-байтовая длина + сообщение) независимо
// от значения network — это и упрощает fake-сервер до одной ветки кадрирования
// (см. net.(*Resolver).exchange в стандартной библиотеке: выбор framing идёт
// по факту реализации интерфейса у Conn, а не по строке "udp"/"tcp").
//
// t.Cleanup проверяет, что Dial вообще был вызван: иначе резолвер мог найти
// ответ в обход заглушки (см. fakeDNSHostname выше), и тест зелёный не по
// той причине — тихая регрессия такого рода уже случалась с "localhost".
func fakeIPResolver(t *testing.T, hostname string, ip net.IP) *net.Resolver {
	t.Helper()
	var dials atomic.Int32
	t.Cleanup(func() {
		if dials.Load() == 0 {
			t.Error("Dial ни разу не вызван: резолюция ушла в обход fake-резолвера " +
				"(например, через /etc/hosts) — тест зелёный не по той причине")
		}
	})
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			client, server := net.Pipe()
			go serveFakeDNSAnswer(server, hostname, ip)
			return client, nil
		},
	}
}

// serveFakeDNSAnswer обслуживает ровно один DNS-запрос на conn: резолвер
// стандартной библиотеки закрывает соединение сразу после одного обмена
// запрос-ответ (см. net.(*Resolver).exchange), так что цикла на incoming
// не нужно. Ошибки чтения/записи (закрытый pipe, отменённый контекст)
// игнорируются — это фоновая горутина, а не тест.
func serveFakeDNSAnswer(conn net.Conn, hostname string, ip net.IP) {
	defer conn.Close()

	lenBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return
	}
	msg := make([]byte, binary.BigEndian.Uint16(lenBuf))
	if _, err := io.ReadFull(conn, msg); err != nil {
		return
	}
	if len(msg) < 12 {
		return
	}

	id := binary.BigEndian.Uint16(msg[0:2])
	name, question, qtype := parseDNSQuestion(msg)
	resp := buildDNSAnswer(id, question, qtype, name, hostname, ip)

	out := make([]byte, 2+len(resp))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(resp)))
	copy(out[2:], resp)
	_, _ = conn.Write(out)
}

// parseDNSQuestion декодирует QNAME (для сверки с ожидаемым hostname), а
// также возвращает сырые байты вопроса (QNAME+QTYPE+QCLASS) для дословного
// эха в ответе и сам QTYPE.
// Границы буфера проверяются на каждом шаге, а не только len(msg) < 12 у
// вызывающего кода: этот разбор бежит в фоновой горутине сервера теста, и
// паника там валит весь тестовый бинарник пакета стеком, не указывающим на
// причину, — вместо неё на любой нехватке байт (обрезанная метка, метки без
// завершающего нуля, недостаточно байт под QTYPE/QCLASS) возвращается пустой
// результат: buildDNSAnswer тогда сам соберёт ответ с нулём записей, как на
// обычный неопознанный запрос.
func parseDNSQuestion(msg []byte) (name string, question []byte, qtype uint16) {
	var labels []string
	i := 12
	for i < len(msg) && msg[i] != 0 {
		l := int(msg[i])
		if i+1+l > len(msg) {
			return "", nil, 0
		}
		labels = append(labels, string(msg[i+1:i+1+l]))
		i += 1 + l
	}
	if i >= len(msg) {
		return "", nil, 0
	}
	i++ // завершающий нулевой байт QNAME
	if i+4 > len(msg) {
		return "", nil, 0
	}
	qtype = binary.BigEndian.Uint16(msg[i : i+2])
	return strings.Join(labels, "."), msg[12 : i+4], qtype
}

// buildDNSAnswer собирает DNS-ответ: заголовок (QR/RD/RA, тот же ID),
// вопрос эхом и, при совпадении имени и qtype=A(1), одну A-запись с ip.
// Для остальных типов (в частности AAAA) отвечает NOERROR с ancount=0 —
// это обычный ответ "нет записи такого типа", а не ошибка.
func buildDNSAnswer(id uint16, question []byte, qtype uint16, gotName, wantName string, ip net.IP) []byte {
	const qtypeA = 1
	var ancount uint16
	var answer []byte
	if v4 := ip.To4(); qtype == qtypeA && v4 != nil && strings.EqualFold(strings.TrimSuffix(gotName, "."), wantName) {
		ancount = 1
		answer = append(answer, 0xC0, 0x0C) // имя — указатель на офсет 12 (начало QNAME)
		answer = binary.BigEndian.AppendUint16(answer, qtypeA)
		answer = binary.BigEndian.AppendUint16(answer, 1)   // CLASS IN
		answer = binary.BigEndian.AppendUint32(answer, 300) // TTL
		answer = binary.BigEndian.AppendUint16(answer, 4)   // RDLENGTH
		answer = append(answer, v4...)
	}

	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], id)
	binary.BigEndian.PutUint16(hdr[2:4], 0x8180) // response=1, rd=1, ra=1, rcode=0
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // qdcount
	binary.BigEndian.PutUint16(hdr[6:8], ancount)

	msg := append(hdr, question...)
	return append(msg, answer...)
}

func TestDNSCheckerAEmptyExpectedOK(t *testing.T) {
	c := uptime.NewDNSChecker()
	c.Resolver = fakeIPResolver(t, fakeDNSHostname, net.ParseIP("127.0.0.1"))
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:   fakeDNSHostname,
		RecordType: "A",
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
}

func TestDNSCheckerAExpectedMatchOK(t *testing.T) {
	c := uptime.NewDNSChecker()
	c.Resolver = fakeIPResolver(t, fakeDNSHostname, net.ParseIP("127.0.0.1"))
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:      fakeDNSHostname,
		RecordType:    "A",
		ExpectedValue: "127.0.0.1",
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
}

func TestDNSCheckerLocalhostAExpectedMismatchFails(t *testing.T) {
	c := uptime.NewDNSChecker()
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:      "localhost",
		RecordType:    "A",
		ExpectedValue: "1.2.3.4",
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
}

func TestDNSCheckerNonexistentDomainFails(t *testing.T) {
	c := uptime.NewDNSChecker()
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:   "nonexistent-domain-gotcha-test.invalid",
		RecordType: "A",
	}))

	got := c.Check(context.Background(), m)
	if got.OK {
		t.Fatalf("Check() = %+v, want fail", got)
	}
	if got.Error == "" {
		t.Errorf("Error is empty, want a message")
	}
}

// --- Dispatcher ---

func TestCheckerForDispatchesByKind(t *testing.T) {
	cases := []struct {
		kind uptime.Kind
		want string
	}{
		{uptime.KindHTTP, "*uptime.HTTPChecker"},
		{uptime.KindTCP, "*uptime.TCPChecker"},
		{uptime.KindDNS, "*uptime.DNSChecker"},
	}
	for _, tc := range cases {
		got, err := uptime.CheckerFor(tc.kind, false)
		if err != nil {
			t.Fatalf("CheckerFor(%v): %v", tc.kind, err)
		}
		if gotType := fmt.Sprintf("%T", got); gotType != tc.want {
			t.Errorf("CheckerFor(%v) = %s, want %s", tc.kind, gotType, tc.want)
		}
	}
}

func TestCheckerForHeartbeatFails(t *testing.T) {
	_, err := uptime.CheckerFor(uptime.KindHeartbeat, false)
	if err == nil {
		t.Fatalf("CheckerFor(heartbeat) = nil error, want error")
	}
}
