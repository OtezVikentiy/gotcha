// Package humanize — единая точка форматирования величин для человека:
// относительное и абсолютное время, длительности, часовые пояса и значения
// метрик регрессий/web-vitals.
//
// Раньше это форматирование было размножено по шести файлам в internal/web и
// internal/trace: `ago` в templates/issues.templ, FormatTime/FormatDuration/
// LocationOrUTC в templates/humantime.go, formatRegressionValue в
// templates/regressions.templ и formatMetric в trace/regression_notify.go.
// Из последней пары независимых копий одна конвертировала duration из
// микросекунд, а вторая — уже нет (после того как задача 1 свела duration к
// миллисекундам в единственной точке, internal/trace.msSample), и завышала
// значения в письмах ровно в 1000 раз. Пакет существует, чтобы такая копия
// больше не могла разойтись: переезд поверхностей — отдельная задача,
// использующая только эти пять функций.
//
// Список выше писался на момент создания пакета и с тех пор устарел: задачи
// C7/C8 того же подпроекта перевели на Time/Duration ещё два места, которых
// в нём нет. prettyBound (templates/timerange.templ, граница произвольного
// диапазона дат из формата datetime-local) делегирует Time вместо
// собственного Format. incidentDurationText — раньше существовавший ДВУМЯ
// независимыми реализациями одного и того же алгоритма длительности
// инцидента, в internal/web/statuspage.go и в templates/monitordetail.templ
// — теперь в обоих местах вызывает Duration напрямую. Ни prettyBound, ни
// incidentDurationText не стали отдельными функциями пакета: обе поверхности
// используют существующие Time/Duration, как и было задумано абзацем выше.
//
// Пакет намеренно лежит ниже internal/web и internal/trace: он не должен
// зависеть ни от одного из них, иначе не сможет использоваться из обоих.
// Единственная внутренняя зависимость — internal/i18n, для тех величин, что
// действительно переводятся (относительное время, длительность).
package humanize

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// CompactNumber — компактная человекочитаемая запись произвольного числа
// (значения и пороги OTLP-метрик): до трёх значащих цифр с суффиксом
// k/M/G/T вместо научной нотации («9.1e+08» → «910M» — QA-находка о
// нечитаемых порогах на странице правил метрик). Значения ниже тысячи
// печатаются без суффикса; совсем мелкие (<0.001) — научной нотацией:
// для них она честнее каши из нулей.
//
// Не локализуется намеренно: суффиксы СИ-подобные и едины для обеих локалей,
// как единицы на осях графиков (см. formatAxisValue в internal/web —
// делегирует сюда).
func CompactNumber(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e12:
		return compactMantissa(v/1e12) + "T"
	case abs >= 1e9:
		return compactMantissa(v/1e9) + "G"
	case abs >= 1e6:
		return compactMantissa(v/1e6) + "M"
	case abs >= 1e3:
		return compactMantissa(v/1e3) + "k"
	case abs > 0 && abs < 0.001:
		return strconv.FormatFloat(v, 'g', 3, 64)
	default:
		return compactMantissa(v)
	}
}

// compactMantissa печатает число с точностью до трёх значащих цифр в
// десятичной записи, срезая хвостовые нули («1.50» → «1.5», «17.0» → «17»).
// Для мантисс суффиксов диапазон [1, 1000); для значений без суффикса
// добавляются разряды после точки вплоть до 0.001.
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

// Ago — локализованное относительное время вида «3 секунды назад»/«5 минут
// назад»/«2 часа назад»/«4 дня назад» (см. plurals time.ago.* в каталогах
// i18n). Отрицательная разница (небольшой перекос часов между БД и веб-узлом)
// приравнивается к нулю, а не показывается как «будущее» время.
//
// Перенесено дословно из internal/web/templates/issues.templ (ago).
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

// Time — момент времени в человекочитаемом виде, в указанном поясе и с его
// названием.
//
// Пояс в подписи обязателен. Раньше разовое окно обслуживания показывалось
// как «2026-07-31T00:00:00Z (Europe/Moscow)»: число в UTC, подпись про
// Москву — и оператор читал время, которого в его поясе не существует.
//
// Формат числовой и одинаковый в обеих локалях намеренно: «2026-07-31 03:00
// MSK» читается однозначно и на русском, и на английском, а таблицы названий
// месяцев добавили бы два перевода ради того же смысла. Поэтому ctx здесь не
// используется внутри функции — параметр принят первым только ради
// единообразия сигнатур пакета (все остальные функции, кроме LocationOrUTC,
// его используют), а не забыт по невнимательности.
//
// Перенесено дословно из internal/web/templates/humantime.go (FormatTime).
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

// Duration — длительность словами на языке интерфейса.
//
// Раньше здесь стоял time.Duration.String(): дежурный видел «23m0s» и
// «1h30m0s» — машинный формат, при том что публичная статус-страница для
// постороннего посетителя показывала «23m» и «5 часов назад». Внутреннему
// пользователю доставалось хуже, чем внешнему.
//
// Показывается одна единица, самая крупная: «2 часа» вместо «2 часа 7 минут».
// Точность здесь не нужна — нужно понять порядок; точное время начала и
// окончания рядом.
//
// Перенесено дословно из internal/web/templates/humantime.go (FormatDuration).
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

// LocationOrUTC разбирает имя пояса, возвращая UTC при неизвестном имени.
// Неизвестный пояс — не повод не показать время вовсе.
//
// Перенесено дословно из internal/web/templates/humantime.go (LocationOrUTC).
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

// MetricValue — значение метрики регрессии человекочитаемо: cls —
// безразмерное отношение (0.25), duration — время в миллисекундах с суффиксом
// до секунды, дальше секунды с одним знаком после запятой (640мс → «640ms»,
// 1180мс → «1.2s»), веб-виталы (lcp/inp/fcp/ttfb и любая неизвестная метрика)
// — миллисекунды с двумя знаками после запятой у секунд, как их показывает
// webvitals.formatVitalMS в остальном интерфейсе.
//
// Объединяет две прежние независимые копии этой функции:
//   - templates.formatRegressionValue (templates/regressions.templ) —
//     верная по СТРУКТУРЕ: она единственная различала cls/duration/веб-виталы,
//     потому что до задачи 1 duration приходил из transactions_5m в
//     микросекундах, а веб-виталы — уже в миллисекундах, и общий формат «как
//     у vitals», применённый к duration напрямую, завышал значения ровно в
//     1000 раз (640мс показывались как «640.00s»). Разделение веток отсюда;
//   - trace.formatMetric (trace/regression_notify.go) — та самая вторая
//     копия, разошедшаяся с первой: она не различала duration и веб-виталы и
//     не конвертировала duration из микросекунд, из-за чего дефект «×1000»
//     дожил именно до писем-уведомлений о регрессиях. Численная формула этой
//     копии (целые миллисекунды / секунды с одним знаком) отсюда — она стала
//     верной для duration именно теперь, когда есть задача 1.
//
// Единственная точка конвертации duration из микросекунд в миллисекунды —
// msSample в internal/trace (задача 1 этого подпроекта). Начиная с неё
// duration приходит на вход этой функции уже готовым, в миллисекундах, —
// поэтому ветка duration ниже не делит на 1000 при переводе единиц, только
// при переходе от «мс» к «с» на отображении. Формулу перцентилей эндпойнтов
// (formatDurationUS, микросекунды, templates/performance.templ) она не
// заменяет — тот источник в эту функцию не заходит.
//
// Ветка duration ниже секунды миллисекунды не показывает: до 1 мс значение
// выводится в микросекундах (форма записи — как у formatDurationUS, но не
// вызов её и не перенос: пакет не может звать templates, а сама формула нужна
// здесь только целым числом с суффиксом «µs»). Это не про полноту, а про то
// же самое искажение, ради которого затеян весь подпроект — только на
// порядок мельче завышения в 1000 раз: быстрый эндпойнт с p95 в 900
// микросекунд при округлении до целых миллисекунд превращается в «0ms», и
// это неправда о работающем быстро эндпойнте, а не более грубая точность.
func MetricValue(ctx context.Context, metric string, v float64) string {
	if v < 0 {
		v = 0
	}
	switch metric {
	case "cls":
		return strconv.FormatFloat(v, 'f', 2, 64)
	case "duration":
		switch {
		case v < 1: // < 1 мс — целых миллисекунд не осталось бы вовсе, показываем µs
			return strconv.FormatFloat(v*1000, 'f', 0, 64) + "µs"
		case v < 1000:
			return strconv.FormatFloat(v, 'f', 0, 64) + "ms"
		default:
			return strconv.FormatFloat(v/1000, 'f', 1, 64) + "s"
		}
	default: // lcp/inp/fcp/ttfb и неизвестные метрики — веб-виталы, как formatVitalMS
		if v < 1000 {
			return strconv.FormatFloat(v, 'f', 0, 64) + "ms"
		}
		return strconv.FormatFloat(v/1000, 'f', 2, 64) + "s"
	}
}
