package uptime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Result — итог одной проверки; поля 1:1 с колонками CH-таблицы
// check_results.
type Result struct {
	OK         bool
	StatusCode int
	Error      string // пусто при OK

	DNSMs, ConnectMs, TLSMs, TTFBMs, TotalMs uint32
	BodySize                                 uint32

	SSLExpiresAt *time.Time // только для https
}

// Checker выполняет одну проверку монитора. Реализации чисты: без БД и без
// Service — только сеть. Ошибки самой проверки (недоступность, таймаут,
// неожиданный код ответа и т.п.) — это не Go-ошибка Check, а Result{OK:
// false, Error: "..."}; Go-ошибка возвращалась бы только при программной
// невозможности выполнить проверку в принципе, чего у этих чекеров не
// бывает.
type Checker interface {
	Check(ctx context.Context, m Monitor) Result
}

// CheckerFor возвращает чекер для kind монитора. kind=heartbeat не
// проверяется активно (ждёт входящих пингов от клиента), поэтому для него
// возвращается ошибка — планировщик не должен пытаться его чекать.
// allowPrivate прокидывается в HTTP/TCP-чекеры: false (по умолчанию у
// Runner/ProbeClient) включает SSRF-фильтр приватных целей.
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

// msToUint32 переводит d в целые миллисекунды, отрицательные значения
// (не должны возникать, но береженого бог бережёт) превращает в 0, а
// значения, не влезающие в uint32, — насыщает math.MaxUint32.
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

// isTimeout сообщает, представляет ли err (возможно, обёрнутую) таймаут —
// используется, чтобы добавить в Result.Error префикс "timeout:", по
// которому его легко отличить от прочих сетевых ошибок.
func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// errMessage форматирует err в текст Result.Error. Для таймаутов возвращает
// конкретный формат "timeout after Ns", если timeoutSeconds > 0; в противном
// случае просто "timeout".
func errMessage(err error, timeoutSeconds int) string {
	if isTimeout(err) {
		if timeoutSeconds > 0 {
			return fmt.Sprintf("timeout after %ds", timeoutSeconds)
		}
		return "timeout"
	}
	return err.Error()
}
