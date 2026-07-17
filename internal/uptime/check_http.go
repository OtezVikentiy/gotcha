package uptime

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/netguard"
)

const (
	httpUserAgent    = "Gotcha-Uptime/1.0"
	maxHTTPBodyBytes = 1 << 20 // 1 MB
)

// HTTPChecker — HTTP(S)-чекер.
//
// SSRF: по умолчанию (AllowPrivate=false) чекер режет соединения к
// приватным/служебным адресам (loopback, RFC1918, link-local/метадата
// облака и т.п.) через netguard — фильтр работает по фактическому IP после
// резолва и на каждом hop редиректа, устойчив к DNS-rebind. Это защита от
// мультитенантного SSRF: один арендатор не должен мониторить внутренние
// сервисы кластера. Оператор может отключить фильтр глобально флагом
// (AllowPrivate=true), если инстанс single-tenant и мониторит свою же
// приватную сеть.
type HTTPChecker struct {
	// AllowPrivate=true отключает SSRF-фильтр приватных целей.
	AllowPrivate bool

	// TLSClientConfig, если задан, используется вместо стандартного
	// TLS-конфига транспорта — нужно тестам, чтобы доверять
	// самоподписанному сертификату httptest.NewTLSServer.
	TLSClientConfig *tls.Config
}

func NewHTTPChecker(allowPrivate bool) *HTTPChecker {
	return &HTTPChecker{AllowPrivate: allowPrivate}
}

// httpTiming собирает моменты событий httptrace для расчёта тайминга по
// фазам запроса.
type httpTiming struct {
	dnsStart, dnsDone         time.Time
	connectStart, connectDone time.Time
	tlsStart, tlsDone         time.Time
	firstByte                 time.Time
}

func (t *httpTiming) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { t.dnsDone = time.Now() },
		ConnectStart:         func(network, addr string) { t.connectStart = time.Now() },
		ConnectDone:          func(network, addr string, err error) { t.connectDone = time.Now() },
		TLSHandshakeStart:    func() { t.tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.tlsDone = time.Now() },
		GotFirstResponseByte: func() { t.firstByte = time.Now() },
	}
}

// durMs возвращает продолжительность между двумя моментами в мс, или 0,
// если одно из событий не произошло (например DNS для литерального IP).
func durMs(start, end time.Time) uint32 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return msToUint32(end.Sub(start))
}

func (c *HTTPChecker) Check(ctx context.Context, m Monitor) Result {
	var cfg HTTPConfig
	if err := strictUnmarshal(m.Config, &cfg); err != nil {
		return Result{Error: fmt.Sprintf("invalid http config: %v", err)}
	}

	var bodyReader io.Reader
	if cfg.Body != "" {
		bodyReader = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bodyReader)
	if err != nil {
		return Result{Error: fmt.Sprintf("build request: %v", err)}
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", httpUserAgent)

	var timing httpTiming
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), timing.clientTrace()))

	// Create a fresh transport with keep-alives disabled. Each check needs a real
	// connection to accurately measure ConnectMs and TLSMs. Since this transport
	// is discarded after each check, pooling would leak idle connections and their
	// goroutines. DisableKeepAlives ensures we get a fresh connect per check.
	transport := &http.Transport{
		DisableKeepAlives: true,
		// SSRF-фильтр: DialContext режет приватные цели по фактическому IP.
		// Для FollowRedirects фильтр срабатывает автоматически на каждом
		// hop — каждый редирект открывает новое соединение через этот же
		// DialContext.
		DialContext: netguard.DialContext(c.AllowPrivate),
	}
	defer transport.CloseIdleConnections()
	if c.TLSClientConfig != nil {
		transport.TLSClientConfig = c.TLSClientConfig
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(m.TimeoutSeconds) * time.Second,
	}
	if !cfg.FollowRedirects {
		// ErrUseLastResponse останавливает следование редиректу и
		// возвращает сам 3xx-ответ клиенту — он и проверяется ниже
		// против ExpectedStatus, как любой другой ответ.
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	total := time.Since(start)
	if err != nil {
		return Result{Error: errMessage(err, m.TimeoutSeconds), TotalMs: msToUint32(total)}
	}
	defer resp.Body.Close()

	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
	if readErr != nil {
		return Result{
			StatusCode: resp.StatusCode,
			Error:      fmt.Sprintf("read body: %v", readErr),
			TotalMs:    msToUint32(total),
		}
	}

	result := Result{
		StatusCode: resp.StatusCode,
		BodySize:   uint32(len(bodyBytes)),
		DNSMs:      durMs(timing.dnsStart, timing.dnsDone),
		ConnectMs:  durMs(timing.connectStart, timing.connectDone),
		TLSMs:      durMs(timing.tlsStart, timing.tlsDone),
		TTFBMs:     durMs(start, timing.firstByte),
		TotalMs:    msToUint32(total),
	}

	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		notAfter := resp.TLS.PeerCertificates[0].NotAfter
		result.SSLExpiresAt = &notAfter
	}

	if !statusExpected(resp.StatusCode, cfg.ExpectedStatus) {
		result.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return result
	}
	if cfg.BodyContains != "" && !bytes.Contains(bodyBytes, []byte(cfg.BodyContains)) {
		result.Error = "body does not contain expected substring"
		return result
	}
	if cfg.BodyNotContains != "" && bytes.Contains(bodyBytes, []byte(cfg.BodyNotContains)) {
		result.Error = "body contains forbidden substring"
		return result
	}

	result.OK = true
	return result
}

// statusExpected сообщает, входит ли code в expected; пустой expected
// означает 200..299.
func statusExpected(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 300
	}
	for _, e := range expected {
		if e == code {
			return true
		}
	}
	return false
}
