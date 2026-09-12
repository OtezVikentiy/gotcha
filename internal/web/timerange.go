package web

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

type TimeRange struct {
	From, To time.Time
	Key      string
	Custom   bool
}

func (tr TimeRange) Window() time.Duration { return tr.To.Sub(tr.From) }

// совпадает с окном хранения событий/трейсов; запрос за его пределами всё равно вернул бы пусто.
const timeRangeRetention = 90 * 24 * time.Hour

// осмысленно не везде — у графика без границ нет оси.
// у списка проблем — дефолт: старые группы иначе спрятались бы за фильтр на здоровом проекте.
const RangeAll = "all"

// источник истины для пресетов — guards читает его ключи напрямую (i18n_dynamic_test.go).
var TimeRangePresets = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// приоритет: start/end → пресет в period → перенесённый custom (cstart/cend) → дефолт def.
func parseTimeRange(q url.Values, def string) TimeRange {
	now := time.Now().UTC()

	// хватает одного start — parseCustomRange берёт конец = «сейчас», если end пуст.
	if q.Get("start") != "" {
		if tr, ok := parseCustomRange(q.Get("start"), q.Get("end"), now); ok {
			return tr
		}
	}

	key := q.Get("period")
	// только при def==RangeAll — иначе график получит нулевое окно: autoStep делит на него.
	if key == RangeAll && def == RangeAll {
		return TimeRange{Key: RangeAll}
	}
	if w, ok := TimeRangePresets[key]; ok {
		return TimeRange{From: now.Add(-w), To: now, Key: key}
	}

	if key == "custom" {
		if tr, ok := parseCustomRange(q.Get("cstart"), q.Get("cend"), now); ok {
			return tr
		}
	}

	if def == RangeAll {
		return TimeRange{Key: RangeAll}
	}
	w := TimeRangePresets[def]
	return TimeRange{From: now.Add(-w), To: now, Key: def}
}

// нормализует: конец не в будущем (по умолчанию «сейчас»), начало строго раньше конца.
// размах не больше окна хранения; ok=false — вызывающий берёт пресет.
func parseCustomRange(startStr, endStr string, now time.Time) (TimeRange, bool) {
	from, ok := parseRangeTime(startStr)
	if !ok {
		return TimeRange{}, false
	}
	to, ok := parseRangeTime(endStr)
	if !ok {
		to = now
	}
	if to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return TimeRange{}, false
	}
	if to.Sub(from) > timeRangeRetention {
		from = to.Add(-timeRangeRetention)
	}
	return TimeRange{From: from.UTC(), To: to.UTC(), Key: "custom", Custom: true}, true
}

// принимает datetime-local, date, RFC3339 или unix-секунды.
// без зоны трактуется как UTC — сервер и хранилище работают в UTC (self-hosted, один тенант).
func parseRangeTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0).UTC(), true
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// align нужен perf-графикам — источник MV transactions_5m, шаг обязан быть кратен 5 минутам.
// округление вверх держит число корзин не больше целевого — важно на произвольных диапазонах.
func autoStep(window, minStep, align time.Duration, buckets int) time.Duration {
	if buckets < 1 {
		buckets = 1
	}
	step := window / time.Duration(buckets)
	if step < minStep {
		step = minStep
	}
	if align > 0 {
		if r := step % align; r != 0 {
			step += align - r
		}
	}
	// CH бакетирует по целым секундам; нецелый шаг рассинхронит его с gapfill на свежем бакете.
	// minStep у всех вызовов ≥ 1m, обрезка не даёт ноль.
	step -= step % time.Second
	return step
}

// поля диапазона заполняются только при Custom — иначе любой submit уводил бы в custom.
func timeRangeVM(tr TimeRange) templates.TimeRangeVM {
	vm := templates.TimeRangeVM{Key: tr.Key, Custom: tr.Custom}
	if tr.Custom {
		vm.Start = timeRangeFieldValue(tr.From)
		vm.End = timeRangeFieldValue(tr.To)
	}
	return vm
}

func timeRangeFieldValue(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04")
}
