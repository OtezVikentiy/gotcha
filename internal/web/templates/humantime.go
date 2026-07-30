package templates

import (
	"context"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// FormatTime — момент времени в человекочитаемом виде, в указанном поясе и с
// его названием.
//
// Пояс в подписи обязателен. Раньше разовое окно обслуживания показывалось как
// «2026-07-31T00:00:00Z (Europe/Moscow)»: число в UTC, подпись про Москву — и
// оператор читал время, которого в его поясе не существует.
//
// Формат числовой и одинаковый в обеих локалях намеренно: «2026-07-31 03:00
// MSK» читается однозначно и на русском, и на английском, а таблицы названий
// месяцев добавили бы два перевода ради того же смысла.
func FormatTime(t time.Time, loc *time.Location) string {
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

// FormatDuration — длительность словами на языке интерфейса.
//
// Раньше здесь стоял time.Duration.String(): дежурный видел «23m0s» и
// «1h30m0s» — машинный формат, при том что публичная статус-страница для
// постороннего посетителя показывала «23m» и «5 часов назад». Внутреннему
// пользователю доставалось хуже, чем внешнему.
//
// Показывается одна единица, самая крупная: «2 часа» вместо «2 часа 7 минут».
// Точность здесь не нужна — нужно понять порядок; точное время начала и
// окончания рядом.
func FormatDuration(ctx context.Context, d time.Duration) string {
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
