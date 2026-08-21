package log

import (
	"strconv"
	"strings"
)

// Канон severity — единый набор уровней, к которому приводится любой источник
// (OTLP SeverityNumber/SeverityText, JSON-логи приложений, syslog). UI и
// правила алертинга работают только с этими шестью значениями, не с сырыми
// строками поставщика.
const (
	SevTrace = "trace"
	SevDebug = "debug"
	SevInfo  = "info"
	SevWarn  = "warn"
	SevError = "error"
	SevFatal = "fatal"
)

// Severities — канон в порядке возрастания серьёзности. Для наполнения
// селекта фильтра severity в UI и валидации входных значений List.
var Severities = []string{SevTrace, SevDebug, SevInfo, SevWarn, SevError, SevFatal}

// CanonFromNumber сводит OTLP SeverityNumber (1-24, см. спецификацию OTel) к
// канону: 1-4 trace, 5-8 debug, 9-12 info, 13-16 warn, 17-20 error, 21-24
// fatal. Число вне диапазона (0, отрицательное, >24) — не ошибка формата
// (поставщик мог прислать мусор), поэтому не роняем запись, а относим её к
// SevInfo: это нейтральный уровень, ничего не теряем и не эскалируем зря.
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

// CanonFromText сводит текстовый уровень (SeverityText OTLP, поле level/severity
// в JSON-логах) к канону. Разные экосистемы называют одно и то же по-разному
// (err/error, warn/warning, fatal/critical) — словарь покрывает обе формы.
// Числовая строка ("17") трактуется как SeverityNumber. Пустая или
// нераспознанная строка — SevInfo по той же причине, что и в CanonFromNumber:
// нейтральный дефолт, не теряем запись из-за незнакомого формата.
func CanonFromText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return SevInfo
	}
	// ParseInt с bitSize=32 вместо Atoi+каста: Atoi возвращает int (64 бита на
	// проде), и int32(n) молча заворачивал бы значения вне диапазона int32
	// (CodeQL #19, incorrect integer conversion). Не влезло в int32 — это не
	// SeverityNumber, падаем в текстовый словарь ниже (итог — SevInfo).
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
