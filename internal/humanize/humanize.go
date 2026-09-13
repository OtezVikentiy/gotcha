package humanize

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// До трёх значащих цифр с суффиксом k/M/G/T; мелкие (<0.001) — научной нотацией, честнее нулей.
// Не локализуется: суффиксы СИ-подобные и едины для обеих локалей (как оси графиков).
func CompactNumber(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e12:
		return compactMantissa(v/1e12) + "T"
	case abs >= 1e9:
		if s, ok := capMantissa(v/1e9, "G"); ok {
			return s
		}
		return compactMantissa(v/1e12) + "T"
	case abs >= 1e6:
		if s, ok := capMantissa(v/1e6, "M"); ok {
			return s
		}
		return compactMantissa(v/1e9) + "G"
	case abs >= 1e3:
		if s, ok := capMantissa(v/1e3, "k"); ok {
			return s
		}
		return compactMantissa(v/1e6) + "M"
	case abs > 0 && abs < 0.001:
		return strconv.FormatFloat(v, 'g', 3, 64)
	default:
		return compactMantissa(v)
	}
}

// Округление у границы разряда (999950 -> "1000") на деле уже следующий
// разряд (999500 -> "1M", не "1000k") — capMantissa ловит переход.
func capMantissa(scaled float64, suffix string) (string, bool) {
	s := compactMantissa(scaled)
	if n, err := strconv.ParseFloat(s, 64); err == nil && math.Abs(n) >= 1000 {
		return "", false
	}
	return s + suffix, true
}

func compactMantissa(v float64) string {
	prec := 2
	switch abs := math.Abs(v); {
	case abs >= 100:
		prec = 0
	case abs >= 10:
		prec = 1
	case abs < 0.01:
		prec = 5
	case abs < 0.1:
		prec = 4
	case abs < 1:
		prec = 3
	}
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// Отрицательная разница (перекос часов БД/веб) приравнивается к нулю, а не показывается как «будущее».
func Ago(ctx context.Context, t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return i18n.T(ctx, "time.just_now")
	case d < time.Minute:
		return i18n.Tn(ctx, "time.ago.seconds", int(d/time.Second))
	case d < time.Hour:
		return i18n.Tn(ctx, "time.ago.minutes", int(d/time.Minute))
	case d < 24*time.Hour:
		return i18n.Tn(ctx, "time.ago.hours", int(d/time.Hour))
	default:
		return i18n.Tn(ctx, "time.ago.days", int(d/(24*time.Hour)))
	}
}

// Пояс в подписи обязателен — раньше время показывали в UTC с чужой подписью пояса, читалось неверно.
// ctx не используется внутри — принят только ради единообразия сигнатур пакета, не по недосмотру.
func Time(ctx context.Context, t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	zone, _ := local.Zone()
	return local.Format("2006-01-02 15:04") + " " + zone
}

// Не сырой time.Duration.String() («23m0s») — внутреннему пользователю было хуже, чем внешнему.
// Одна единица, самая крупная («2 часа», не «2 часа 7 минут») — точность не нужна, важен порядок.
func Duration(ctx context.Context, d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= 24*time.Hour:
		return i18n.Tn(ctx, "unit.days", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return i18n.Tn(ctx, "unit.hours", int(d/time.Hour))
	case d >= time.Minute:
		return i18n.Tn(ctx, "unit.minutes", int(d/time.Minute))
	case d >= time.Second:
		return i18n.Tn(ctx, "unit.seconds", int(d/time.Second))
	default:
		return i18n.T(ctx, "time.now")
	}
}

// Неизвестный пояс — не повод не показать время вовсе, поэтому UTC, а не ошибка.
func LocationOrUTC(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// duration приходит уже в миллисекундах — конвертация из микросекунд происходит раньше, в msSample.
// Sub-1мс показывается в µs, не «0ms» — иначе быстрый эндпойнт выглядел бы как нулевой.
func MetricValue(ctx context.Context, metric string, v float64) string {
	if v < 0 {
		v = 0
	}
	switch metric {
	case "cls":
		return strconv.FormatFloat(v, 'f', 2, 64)
	case "duration":
		if v < 1 {
			return strconv.FormatFloat(v*1000, 'f', 0, 64) + "µs"
		}
		if v < 1000 {
			if s, ok := capMS(v); ok {
				return s
			}
		}
		return strconv.FormatFloat(v/1000, 'f', 1, 64) + "s"
	default: // lcp/inp/fcp/ttfb и неизвестные метрики — веб-виталы, как formatVitalMS
		if v < 1000 {
			if s, ok := capMS(v); ok {
				return s
			}
		}
		return strconv.FormatFloat(v/1000, 'f', 2, 64) + "s"
	}
}

// Округление у границы (999.7 -> "1000") на деле уже секунды; вызывающий
// сам считает v/1000 — форматы секунд у duration/веб-виталов различаются.
func capMS(v float64) (string, bool) {
	s := strconv.FormatFloat(v, 'f', 0, 64)
	if s == "1000" {
		return "", false
	}
	return s + "ms", true
}

// Единственная реализация в проекте — не копировать: разные копии одного форматирования разъезжались.
// Не локализуется, отрицательное клэмпится к нулю — тот же приём, что у MetricValue/CompactNumber.
// KiB/MiB/GiB, не KB/MB/GB: делим на 1024, не на 1000 — подпись обязана называть то, что считает код.
func Bytes(b int64) string {
	if b < 0 {
		b = 0
	}
	const unit = 1024
	switch {
	case b >= unit*unit*unit:
		return capUnit(float64(b)/(unit*unit*unit), 2, "GiB")
	case b >= unit*unit:
		if s, ok := capUnitOrPromote(float64(b)/(unit*unit), 1, "MiB"); ok {
			return s
		}
		return capUnit(float64(b)/(unit*unit*unit), 2, "GiB")
	case b >= unit:
		if s, ok := capUnitOrPromote(float64(b)/unit, 1, "KiB"); ok {
			return s
		}
		return capUnit(float64(b)/(unit*unit), 1, "MiB")
	default:
		return strconv.FormatInt(b, 10) + "B"
	}
}

func capUnit(v float64, prec int, suffix string) string {
	return strconv.FormatFloat(v, 'f', prec, 64) + suffix
}

// Округление у самой границы разряда (1048575Б -> 1024.0КиБ) на деле уже
// принадлежит следующему: та же поправка, что у capMS/capMantissa.
func capUnitOrPromote(scaled float64, prec int, suffix string) (string, bool) {
	s := strconv.FormatFloat(scaled, 'f', prec, 64)
	if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 1024 {
		return "", false
	}
	return s + suffix, true
}
