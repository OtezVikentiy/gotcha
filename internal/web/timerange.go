package web

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// TimeRange — единое окно времени для графиков всех разделов. Пришло на смену
// четырём почти одинаковым *PeriodWindow: раньше каждая страница парсила
// query-параметр period сама, со своим списком значений и своим шагом корзины.
// Query-слой (event/trace/metric/uptime) уже везде принимает абсолютные
// from/to, поэтому произвольный диапазон почти не трогает БД-слой.
type TimeRange struct {
	From, To time.Time
	// Key — активный пресет ("1h"/"24h"/"7d"/"30d") либо "custom" для
	// произвольного диапазона. Используется шаблоном для подсветки выбора.
	Key    string
	Custom bool
}

// Window — длительность окна (To-From), из неё autoStep выбирает шаг корзины.
func (tr TimeRange) Window() time.Duration { return tr.To.Sub(tr.From) }

// timeRangeRetention — максимальный размах произвольного диапазона: совпадает с
// окном хранения событий/трейсов (90 дней). Запрос за его пределами всё равно
// вернул бы пусто, поэтому from подтягивается вперёд, а не отдаётся как есть.
const timeRangeRetention = 90 * 24 * time.Hour

// timeRangePresets — пресет → длительность окна назад от «сейчас».
var timeRangePresets = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// timeRangePresetOrder — порядок пресетов в селекторе (map не упорядочен).
var timeRangePresetOrder = []string{"1h", "24h", "7d", "30d"}

// parseTimeRange разбирает окно времени из query-параметров: пресет period или
// произвольный диапазон start/end. Приоритет — у известного пресета в period:
// он выигрывает у оставшихся в форме start/end (после переключения с custom на
// пресет поля даты ещё держат старые значения, но выбор пресета должен победить).
// Только period=custom (или пустой period при заполненных start/end) уводит в
// произвольный диапазон; битый диапазон тихо падает на пресет по умолчанию def.
func parseTimeRange(q url.Values, def string) TimeRange {
	now := time.Now().UTC()

	key := q.Get("period")
	if w, ok := timeRangePresets[key]; ok {
		return TimeRange{From: now.Add(-w), To: now, Key: key}
	}

	if key == "custom" || q.Get("start") != "" || q.Get("end") != "" {
		if tr, ok := parseCustomRange(q, now); ok {
			return tr
		}
	}

	w := timeRangePresets[def]
	return TimeRange{From: now.Add(-w), To: now, Key: def}
}

// parseCustomRange собирает произвольный диапазон из start/end. Нормализует:
// end не в будущем (по умолчанию — «сейчас»), start строго раньше end, размах
// не больше окна хранения. Возвращает ok=false, если start не распарсился или
// диапазон вырожден — тогда вызывающий берёт пресет по умолчанию.
func parseCustomRange(q url.Values, now time.Time) (TimeRange, bool) {
	from, ok := parseRangeTime(q.Get("start"))
	if !ok {
		return TimeRange{}, false
	}
	to, ok := parseRangeTime(q.Get("end"))
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

// parseRangeTime разбирает границу произвольного диапазона, терпимо к формату:
// значение <input type="datetime-local"> ("2006-01-02T15:04"), date-инпута
// ("2006-01-02"), полный RFC3339 или unix-секунды. Время без зоны трактуется
// как UTC — сервер и хранилище работают в UTC (self-hosted, один тенант).
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

// autoStep выбирает шаг корзины для окна window, целясь примерно в buckets
// столбиков, но не мельче minStep и (при align>0) кратно align. align нужен
// perf-графикам: их источник — MV transactions_5m, поэтому шаг обязан быть
// кратен 5 минутам; метрики читают сырьё и передают align=0. Округление вверх
// (а не вниз) держит число корзин не больше целевого — важно на произвольных
// диапазонах, где window/buckets не ложится ровно на границу align.
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
	return step
}

// timeRangeVM строит вью-модель селектора из окна. Поля произвольного
// диапазона заполняются только когда он активен: на пресете они пусты, иначе
// любой submit формы уводило бы в custom (см. parseTimeRange).
func timeRangeVM(tr TimeRange) templates.TimeRangeVM {
	vm := templates.TimeRangeVM{Key: tr.Key, Custom: tr.Custom}
	if tr.Custom {
		vm.Start = timeRangeFieldValue(tr.From)
		vm.End = timeRangeFieldValue(tr.To)
	}
	return vm
}

// timeRangeFieldValue форматирует границу окна для value= в <input
// type="datetime-local"> (минутная точность, без зоны). Пустая строка для
// нулевого времени, чтобы поля произвольного диапазона на пресетах не
// заполнялись (иначе любой submit уводило бы в custom).
func timeRangeFieldValue(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04")
}
