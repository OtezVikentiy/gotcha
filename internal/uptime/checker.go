package uptime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// поля 1:1 с колонками CH-таблицы check_results.
type Result struct {
	OK         bool
	StatusCode int
	Error      string // пусто при OK

	DNSMs, ConnectMs, TLSMs, TTFBMs, TotalMs uint32
	BodySize                                 uint32

	SSLExpiresAt *time.Time // только для https
}

// реализации чисты — без БД/Service, только сеть; сбои проверки
// (недоступность, таймаут) это Result{OK:false}, не Go-ошибка Check.
type Checker interface {
	Check(ctx context.Context, m Monitor) Result
}

// kind=heartbeat не проверяется активно — ждёт входящих пингов, поэтому
// возвращает ошибку; allowPrivate=false (дефолт) включает SSRF-фильтр.
func CheckerFor(kind Kind, allowPrivate bool) (Checker, error) {
	switch kind {
	case KindHTTP:
		return NewHTTPChecker(allowPrivate), nil
	case KindTCP:
		return NewTCPChecker(allowPrivate), nil
	case KindDNS:
		return NewDNSChecker(), nil
	case KindHeartbeat:
		return nil, fmt.Errorf("uptime: heartbeat monitors are not actively checked")
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidMonitor, kind)
	}
}

// var, не const — тесты понижают её, чтобы не ждать реальную секунду.
var retryDelay = time.Second

// гасит транзиентные сбои на уровне одной проверки — в отличие от
// FailThreshold, который считает уже записанные сбои подряд.
func checkWithRetries(ctx context.Context, checker Checker, m Monitor) Result {
	res := checker.Check(ctx, m)
	for i := 0; i < m.Retries && !res.OK; i++ {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(retryDelay):
		}
		res = checker.Check(ctx, m)
	}
	return res
}

// отрицательные значения (не должны возникать) → 0; переполнение uint32 → MaxUint32.
func msToUint32(d time.Duration) uint32 {
	ms := d.Milliseconds()
	switch {
	case ms < 0:
		return 0
	case ms > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(ms)
	}
}

func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// таймаут с известным timeoutSeconds → "timeout after Ns", иначе просто "timeout".
func errMessage(err error, timeoutSeconds int) string {
	if isTimeout(err) {
		if timeoutSeconds > 0 {
			return fmt.Sprintf("timeout after %ds", timeoutSeconds)
		}
		return "timeout"
	}
	return err.Error()
}
