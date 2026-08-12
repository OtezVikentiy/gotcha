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

// RangeAll — окно «за всё время»: границ нет.
//
// Осмысленно не везде: у графика без границ нет оси, а у списка проблем это
// естественный вид по умолчанию — большинство групп старше суток, и прятать их
// за фильтр значит показывать пустой список на здоровом проекте.
const RangeAll = "all"

// TimeRangePresets — пресет → длительность окна назад от «сейчас».
// Экспортирован: это источник истины для множества пресетов — правило
// guards, проверяющее наличие переводов "range.<пресет>", читает его ключи
// напрямую вместо повторения списка (см. internal/guards/i18n_dynamic_test.go).
var TimeRangePresets = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// parseTimeRange разбирает окно времени из query-параметров. Приоритеты
// подобраны так, чтобы произвольный диапазон включался БЕЗ отдельного выбора
// «свой диапазон» в списке (лишнее действие), и при этом переключение
// custom→пресет и сохранение custom при смене прочих фильтров работали без JS:
//
//  1. Видимые поля start+end заполнены → произвольный диапазон (пользователь
//     просто ввёл даты — этого достаточно, пресет в списке не важен).
//  2. Иначе известный пресет в period → он (выбор пресета в списке уводит с
//     произвольного диапазона: скрытый перенос custom из п.3 при этом
//     игнорируется, т.к. period уже не "custom").
//  3. Иначе period=custom + скрытые cstart/cend → перенесённый активный
//     произвольный диапазон (чтобы смена окружения/сортировки его не сбросила;
//     видимые поля при активном custom пусты — под ввод НОВОГО диапазона).
//  4. Иначе пресет по умолчанию def; битый диапазон тоже падает сюда.
func parseTimeRange(q url.Values, def string) TimeRange {
	now := time.Now().UTC()

	// Достаточно заполненного «начала»: parseCustomRange по умолчанию берёт
	// конец = «сейчас», поэтому «с X и до сих пор» — валидный диапазон без
	// отдельного ввода конца (раньше при пустом end этот ввод молча терялся и
	// страница уходила на пресет).
	if q.Get("start") != "" {
		if tr, ok := parseCustomRange(q.Get("start"), q.Get("end"), now); ok {
			return tr
		}
	}

	key := q.Get("period")
	// «За всё время» — отдельный ключ, а не пустая строка: список проблем
	// показывает историю целиком по умолчанию, и этот выбор должен переживать
	// смену сортировки и окружения так же, как пресеты. Границы пустые.
	//
	// Честен только когда def == RangeAll — то есть у страницы, которая сама
	// это предлагает (issues.go, единственная с AllowAll=true в селекторе).
	// Остальные страницы (графики метрик/аптайма/perf/профилей) её не
	// показывают в UI вообще, но query-параметр читает та же parseTimeRange —
	// без этой проверки ?period=all, набранный руками в адресной строке, всё
	// равно долетал сюда и отдавал TimeRange{Key: RangeAll} с нулевыми
	// From/To. У графика окно нужно ВСЕГДА (autoStep делит на него, ось X
	// строится по нему), а запрос в БД с from==to==год-1 не находит ни одной
	// строки — так «за всё время» превращалось в тихую пустую страницу
	// (P1-6), а не в ошибку и не в реальные данные. Раз ни один из этих
	// хэндлеров не умеет рисовать график без оси, находка закрывается тут:
	// период откатывается на дефолт страницы, как и любой другой
	// нераспознанный period.
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

// parseCustomRange собирает произвольный диапазон из строк начала/конца.
// Нормализует: конец не в будущем (по умолчанию — «сейчас»), начало строго
// раньше конца, размах не больше окна хранения. ok=false, если начало не
// распарсилось или диапазон вырожден — тогда вызывающий берёт пресет.
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
	// CH бакетит по ЦЕЛЫМ секундам (toStartOfInterval, INTERVAL stepSec second),
	// а gapfill заливает сетку тем же шагом. Обрезаем до целых секунд, чтобы обе
	// сетки совпадали по построению — иначе при нецелом шаге (произвольное окно,
	// не делящееся на число бакетов) самый свежий бакет не находил соответствия
	// в данных и рисовался как «нет данных». minStep у всех вызовов ≥ 1m, так что
	// обрезка не может дать ноль.
	step -= step % time.Second
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
