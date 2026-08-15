package agent

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSendClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		want       SendResult
		wantFloor  time.Duration
		// wantErrCode — код статуса, который должен встретиться в err.Error()
		// (диагностика для логов runner'а, см. respError в sender.go); "" —
		// на 2xx err обязан быть nil.
		wantErrCode string
	}{
		{"успех", 200, "", SendOK, 0, ""},
		{"5xx — ретрай", 502, "", SendRetry, 0, "502"},
		{"rate-limit", 429, "1", SendRetry, time.Second, "429"},
		{"месячная квота", 429, "2592000", SendRetry, time.Hour, "429"}, // кап 1ч (спека §1.3)
		{"отозванный ключ", 401, "", SendDrop, 0, "401"},
		{"битый payload", 400, "", SendDrop, 0, "400"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/metrics" {
					t.Errorf("путь = %q, хочу /v1/metrics", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Errorf("Authorization = %q, хочу Bearer test-key", got)
				}
				if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
					t.Errorf("Content-Type = %q, хочу application/x-protobuf", got)
				}
				if got := r.Header.Get("Content-Encoding"); got != "gzip" {
					t.Errorf("Content-Encoding = %q, хочу gzip", got)
				}
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			s, err := NewSender(Config{Endpoint: srv.URL, Key: "test-key"})
			if err != nil {
				t.Fatalf("NewSender: %v", err)
			}
			got, floor, sendErr := s.Send(context.Background(), []byte("body"))
			if tc.wantErrCode == "" {
				if sendErr != nil {
					t.Fatalf("Send: %v, хочу nil err (2xx)", sendErr)
				}
			} else {
				if sendErr == nil {
					t.Fatal("Send: err == nil, хочу диагностику со статусом")
				}
				if !strings.Contains(sendErr.Error(), tc.wantErrCode) {
					t.Errorf("Send err = %q, хочу подстроку %q", sendErr.Error(), tc.wantErrCode)
				}
			}
			if got != tc.want {
				t.Errorf("result = %v, хочу %v", got, tc.want)
			}
			if floor != tc.wantFloor {
				t.Errorf("floor = %v, хочу %v", floor, tc.wantFloor)
			}
		})
	}
}

// TestSendConnectionRefused — обрыв соединения (сервер закрыт до запроса)
// классифицируется как SendRetry: сеть недоступна временно, не вина батча.
func TestSendConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	s, err := NewSender(Config{Endpoint: srv.URL, Key: "test-key"})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	got, _, err := s.Send(context.Background(), []byte("body"))
	if err == nil {
		t.Fatal("хочу ошибку сети")
	}
	if got != SendRetry {
		t.Errorf("result = %v, хочу SendRetry", got)
	}
}

// TestSendWithCACert — CACert грузится в свой x509.CertPool (RootCAs) и
// используется для проверки TLS-сертификата инстанса.
func TestSendWithCACert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	certPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(certPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	s, err := NewSender(Config{Endpoint: srv.URL, Key: "test-key", CACert: certPath})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	got, _, err := s.Send(context.Background(), []byte("body"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != SendOK {
		t.Errorf("result = %v, хочу SendOK", got)
	}
}

// TestSendInsecureSkipVerify — без CACert, но с InsecureSkipVerify: TLS без
// проверки сертификата (крайнее средство, спека допускает).
func TestSendInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := NewSender(Config{Endpoint: srv.URL, Key: "test-key", InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	got, _, err := s.Send(context.Background(), []byte("body"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != SendOK {
		t.Errorf("result = %v, хочу SendOK", got)
	}
}

// TestNewSenderClonesDefaultTransport — транспорт собирается клонированием
// http.DefaultTransport (не &http.Transport{} с нуля), поэтому уносит с собой
// Proxy: http.ProxyFromEnvironment (HTTP_PROXY/HTTPS_PROXY/NO_PROXY) и
// дефолтные настройки пула соединений — на голом транспорте они бы молча
// потерялись.
func TestNewSenderClonesDefaultTransport(t *testing.T) {
	s, err := NewSender(Config{Endpoint: "https://example.invalid", Key: "test-key"})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	tr, ok := s.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, хочу *http.Transport", s.client.Transport)
	}
	defTr := http.DefaultTransport.(*http.Transport)

	if tr.Proxy == nil {
		t.Error("Proxy = nil, хочу http.ProxyFromEnvironment (унаследован от DefaultTransport)")
	} else if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(defTr.Proxy).Pointer() {
		t.Error("Proxy не совпадает с http.DefaultTransport.Proxy — HTTP_PROXY/HTTPS_PROXY/NO_PROXY будут проигнорированы")
	}
	if tr.MaxIdleConns != defTr.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, хочу унаследованный дефолт %d", tr.MaxIdleConns, defTr.MaxIdleConns)
	}
	if tr.IdleConnTimeout != defTr.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, хочу унаследованный дефолт %v", tr.IdleConnTimeout, defTr.IdleConnTimeout)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, хочу собранный NewSender tls.Config")
	}
}

func TestNewSenderBadCACert(t *testing.T) {
	_, err := NewSender(Config{Endpoint: "https://example.invalid", Key: "k", CACert: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("хочу ошибку чтения CACert")
	}
}
