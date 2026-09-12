package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type SendResult int

const (
	SendOK    SendResult = iota // 2xx — доставлено
	SendRetry                   // сетевая ошибка / 5xx / 429 — временно, есть смысл повторить
	SendDrop                    // остальное (401/400/403/404/413...) — повтор не поможет
)

const (
	sendTimeout  = 30 * time.Second
	metricsPath  = "/v1/metrics"
	maxRetryWait = time.Hour // месячная квота (2592000с) не должна держать буфер сутками

	// Кап на тело не-2xx ответа для логов — сервер шлёт короткий JSON-error,
	// 512 байт с запасом.
	maxErrBodyLog = 512
)

type Sender struct {
	cfg    Config
	client *http.Client
}

// CACert (если задан) — свой x509.CertPool, ошибка чтения/разбора роняет
// конструктор. InsecureSkipVerify — крайнее средство при отсутствии CACert.
func NewSender(cfg Config) (*Sender, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CACert != "" {
		pemBytes, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CACert %q: %w", cfg.CACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("CACert %q: failed to parse PEM", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
		tlsCfg.InsecureSkipVerify = false // явный CA сильнее общего skip-verify
	}
	// Клон DefaultTransport, не &http.Transport{} с нуля — голый транспорт
	// теряет Proxy: http.ProxyFromEnvironment и дефолтные настройки пула.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Sender{
		cfg: cfg,
		client: &http.Client{
			Timeout:   sendTimeout,
			Transport: tr,
		},
	}, nil
}

// Второе значение — пол ретрая из Retry-After (сервер шлёт секунды, см.
// internal/ingest/handler.go), 0 если заголовка нет; капается в maxRetryWait.
func (s *Sender) Send(ctx context.Context, body []byte) (SendResult, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint+metricsPath, bytes.NewReader(body))
	if err != nil {
		return SendDrop, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Key)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := s.client.Do(req)
	if err != nil {
		return SendRetry, 0, err
	}
	defer func() {
		// respError читает не больше maxErrBodyLog байт — здесь дочитываем
		// остаток, чтобы соединение осталось keep-alive.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		return SendOK, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return SendRetry, retryAfter(resp.Header.Get("Retry-After")), respError(resp)
	default:
		return SendDrop, 0, respError(resp)
	}
}

// err и classification (SendResult) намеренно не смешаны: сервер отдаёт
// строгое подмножество исходов, а err различает причину для оператора.
func respError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyLog))
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, trimmed)
	}
	return fmt.Errorf("server returned %d", resp.StatusCode)
}

// Retry-After как число секунд, не HTTP-дата (сервер шлёт только секунды,
// см. internal/ingest/handler.go); капается в maxRetryWait.
func retryAfter(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return 0
	}
	d := time.Duration(sec) * time.Second
	if d > maxRetryWait {
		return maxRetryWait
	}
	return d
}
