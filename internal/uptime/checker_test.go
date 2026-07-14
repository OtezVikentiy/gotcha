package uptime_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

	c := uptime.NewHTTPChecker()
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

// --- TCP ---

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

	c := uptime.NewTCPChecker()
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

	c := uptime.NewTCPChecker()
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

func TestDNSCheckerLocalhostAEmptyExpectedOK(t *testing.T) {
	c := uptime.NewDNSChecker()
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:   "localhost",
		RecordType: "A",
	}))

	got := c.Check(context.Background(), m)
	if !got.OK || got.Error != "" {
		t.Fatalf("Check() = %+v, want OK", got)
	}
}

func TestDNSCheckerLocalhostAExpectedMatchOK(t *testing.T) {
	c := uptime.NewDNSChecker()
	m := checkerMonitor(uptime.KindDNS, 5, dnsConfig(t, uptime.DNSConfig{
		Hostname:      "localhost",
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
		got, err := uptime.CheckerFor(tc.kind)
		if err != nil {
			t.Fatalf("CheckerFor(%v): %v", tc.kind, err)
		}
		if gotType := fmt.Sprintf("%T", got); gotType != tc.want {
			t.Errorf("CheckerFor(%v) = %s, want %s", tc.kind, gotType, tc.want)
		}
	}
}

func TestCheckerForHeartbeatFails(t *testing.T) {
	_, err := uptime.CheckerFor(uptime.KindHeartbeat)
	if err == nil {
		t.Fatalf("CheckerFor(heartbeat) = nil error, want error")
	}
}
