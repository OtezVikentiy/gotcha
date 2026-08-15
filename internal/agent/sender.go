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

// SendResult — классификация исхода Send: определяет, что делает вызывающий
// (T7) с батчем дальше — буферизовать на повтор или отбросить навсегда.
type SendResult int

const (
	SendOK    SendResult = iota // 2xx — доставлено
	SendRetry                   // сетевая ошибка / 5xx / 429 — временно, есть смысл повторить
	SendDrop                    // остальное (401/400/403/404/413...) — повтор не поможет
)

const (
	sendTimeout  = 30 * time.Second
	metricsPath  = "/v1/metrics"
	maxRetryWait = time.Hour // кап на Retry-After — спека §1.3: месячная квота (2592000с) не должна держать буфер сутками

	// maxErrBodyLog — кап на тело не-2xx ответа, которое попадает в err для
	// логов runner'а (run.go): сервер шлёт короткий JSON-error, 512 байт с
	// запасом (см. respError).
	maxErrBodyLog = 512
)

// Sender — HTTP-клиент push метрик на инстанс Gotcha.
type Sender struct {
	cfg    Config
	client *http.Client
}

// NewSender строит http.Client с фиксированным таймаутом и TLS-настройками
// из Config: CACert (если задан) читается сразу — свой x509.CertPool в
// RootCAs для самоподписанных инстансов, ошибка чтения/разбора — ошибка
// конструктора, а не тихий фолбэк. InsecureSkipVerify — крайнее средство,
// применяется только когда CACert не задан.
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
	// Клонируем http.DefaultTransport, а не строим &http.Transport{} с нуля:
	// голый транспорт теряет Proxy: http.ProxyFromEnvironment (агент в закрытой
	// сети за HTTP_PROXY/HTTPS_PROXY/NO_PROXY просто перестаёт слать метрики —
	// без единой ошибки в логе, транспорт молча идёт напрямую) и дефолтные
	// настройки пула соединений (DialContext-таймауты, MaxIdleConns и т.д.).
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

// Send отправляет уже готовое (gzip+protobuf, см. EncodeBody) тело батча.
// Второе возвращаемое значение — пол ретрая из заголовка Retry-After
// (сервер шлёт только число секунд, см. internal/ingest/handler.go), 0 если
// заголовка нет; капается в maxRetryWait.
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
		// respError (ниже) читает не больше maxErrBodyLog байт тела для
		// диагностики; здесь дочитываем остаток и закрываем, чтобы соединение
		// можно было переиспользовать (keep-alive).
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

// respError строит диагностическую ошибку не-2xx ответа: err — это то, что
// видит оператор в логах runner'а (run.go), classification (SendResult)
// определяет автомат отправки — они намеренно не смешаны: сервер отдаёт
// строгое подмножество исходов (2xx/429/5xx/остальное), а err обязан
// отличать «отозванный ключ» от «битый payload» от «квота» в консоли.
func respError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyLog))
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, trimmed)
	}
	return fmt.Errorf("server returned %d", resp.StatusCode)
}

// retryAfter парсит Retry-After как число секунд (HTTP-дата сервером не
// используется — см. internal/ingest/handler.go) и капает в maxRetryWait.
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
