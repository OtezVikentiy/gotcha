package log

import (
	"strconv"
	"strings"
)

// Единый набор уровней, к которому приводится любой источник (OTLP/JSON/syslog) — UI и алертинг работают
// только с ним, не с сырыми строками поставщика.
const (
	SevTrace = "trace"
	SevDebug = "debug"
	SevInfo  = "info"
	SevWarn  = "warn"
	SevError = "error"
	SevFatal = "fatal"
)

// Канон в порядке возрастания серьёзности — для селекта фильтра в UI и валидации входных значений List.
var Severities = []string{SevTrace, SevDebug, SevInfo, SevWarn, SevError, SevFatal}

// 1-4 trace, 5-8 debug, 9-12 info, 13-16 warn, 17-20 error, 21-24 fatal (OTel SeverityNumber 1-24).
// Вне диапазона — не ошибка формата, относим к SevInfo как нейтральному уровню.
func CanonFromNumber(n int32) string {
	switch {
	case n >= 1 && n <= 4:
		return SevTrace
	case n >= 5 && n <= 8:
		return SevDebug
	case n >= 9 && n <= 12:
		return SevInfo
	case n >= 13 && n <= 16:
		return SevWarn
	case n >= 17 && n <= 20:
		return SevError
	case n >= 21 && n <= 24:
		return SevFatal
	default:
		return SevInfo
	}
}

// Разные экосистемы называют одно и то же по-разному (err/error, warn/warning, fatal/critical) — словарь
// покрывает обе формы. Числовая строка трактуется как SeverityNumber; пустая/нераспознанная — SevInfo.
func CanonFromText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return SevInfo
	}
	// ParseInt с bitSize=32, не Atoi+каст — Atoi даёт int (64 бита на проде), int32(n) молча заворачивал бы
	// значения вне диапазона; не влезло — не SeverityNumber, падаем в словарь ниже.
	if n, err := strconv.ParseInt(s, 10, 32); err == nil {
		return CanonFromNumber(int32(n))
	}
	switch s {
	case "trace":
		return SevTrace
	case "debug":
		return SevDebug
	case "info":
		return SevInfo
	case "warn", "warning":
		return SevWarn
	case "error", "err":
		return SevError
	case "fatal", "critical":
		return SevFatal
	default:
		return SevInfo
	}
}
