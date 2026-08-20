package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrInvalidWindow = errors.New("uptime: invalid maintenance window")

// Подпричины ErrInvalidWindow — каждая validateWindow-ветка оборачивает и
// ErrInvalidWindow (для существующих errors.Is(err, ErrInvalidWindow) —
// см. web/maintenance.go), и одну из этих сентинелей (fmt.Errorf с двумя
// %w, Go 1.20+): вызывающий может отличить конкретную причину через
// errors.Is, не парся текст ошибки (который к тому же был бы на английском
// от time.LoadLocation/time.Parse — P2-1 usability-аудита 2026-08-12).
var (
	ErrInvalidWindowName      = errors.New("uptime: maintenance window name required")
	ErrInvalidWindowTimezone  = errors.New("uptime: invalid maintenance window timezone")
	ErrInvalidWindowWeekday   = errors.New("uptime: invalid maintenance window weekday")
	ErrInvalidWindowStartTime = errors.New("uptime: invalid maintenance window start_time")
	ErrInvalidWindowEndTime   = errors.New("uptime: invalid maintenance window end_time")
	ErrInvalidWindowSameTime  = errors.New("uptime: maintenance window start_time and end_time must differ")
	ErrInvalidWindowRange     = errors.New("uptime: maintenance window starts_at must be before ends_at")
)

// Window — окно обслуживания проекта: разовое (StartsAt/EndsAt) либо
// еженедельное (Weekday + StartTime/EndTime "15:04" в Timezone).
type Window struct {
	ID        int64
	ProjectID int64
	Name      string
	Weekly    bool
	StartsAt  *time.Time
	EndsAt    *time.Time
	Weekday   int
	StartTime string // "15:04"
	EndTime   string // "15:04"
	Timezone  string
}

// parseHHMM parses a "15:04" wall-clock time, rejecting anything else
// (including seconds, AM/PM, or out-of-range hour/minute).
func parseHHMM(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}

func hhmmToPgTime(s string) (pgtype.Time, error) {
	h, m, err := parseHHMM(s)
	if err != nil {
		return pgtype.Time{}, err
	}
	return pgtype.Time{Microseconds: (int64(h)*60 + int64(m)) * 60_000_000, Valid: true}, nil
}

func pgTimeToHHMM(t pgtype.Time) string {
	if !t.Valid {
		return ""
	}
	totalMinutes := t.Microseconds / 60_000_000
	return fmt.Sprintf("%02d:%02d", totalMinutes/60, totalMinutes%60)
}

// validateWindow checks the window is well-formed before it ever reaches
// the DB: the timezone must be a real IANA name (time.LoadLocation), and
// depending on Weekly either the one-off range or the weekday+HH:MM pair
// must be present and sane. This mirrors — but is stricter in the TZ case
// than — the maintenance_windows CHECK constraint, which only enforces the
// one-off-vs-weekly shape.
func validateWindow(w Window) error {
	if w.Name == "" {
		return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowName)
	}
	if _, err := time.LoadLocation(w.Timezone); err != nil {
		return fmt.Errorf("%w: %w: %v", ErrInvalidWindow, ErrInvalidWindowTimezone, err)
	}
	if w.Weekly {
		if w.Weekday < 0 || w.Weekday > 6 {
			return fmt.Errorf("%w: %w: weekday must be 0..6", ErrInvalidWindow, ErrInvalidWindowWeekday)
		}
		if _, _, err := parseHHMM(w.StartTime); err != nil {
			return fmt.Errorf("%w: %w: %v", ErrInvalidWindow, ErrInvalidWindowStartTime, err)
		}
		if _, _, err := parseHHMM(w.EndTime); err != nil {
			return fmt.Errorf("%w: %w: %v", ErrInvalidWindow, ErrInvalidWindowEndTime, err)
		}
		// Одноразовые окна уже требуют EndsAt.After(StartsAt) — тот же запрет
		// нужен и здесь: StartTime == EndTime не задаёт «мгновенное» окно, а
		// молча трактуется windowDuration как полные 24 часа (переход через
		// полночь), так что опечатка при вводе времени блэкаутит уведомления/
		// аптайм на весь день недели без единого сигнала об ошибке.
		if w.StartTime == w.EndTime {
			return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowSameTime)
		}
		return nil
	}
	// «Бессрочно»: EndsAt == nil означает открытое окно, StartsAt всё равно
	// обязателен — иначе не от чего было бы вести активность (windowActive).
	if w.StartsAt == nil {
		return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowRange)
	}
	if w.EndsAt != nil && !w.EndsAt.After(*w.StartsAt) {
		return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowRange)
	}
	return nil
}

// CreateWindow validates and creates a maintenance window.
func (s *Service) CreateWindow(ctx context.Context, w Window) (Window, error) {
	if err := validateWindow(w); err != nil {
		return Window{}, err
	}

	var startsAt, endsAt *time.Time
	var weekday *int
	var startTime, endTime *pgtype.Time
	if w.Weekly {
		wd := w.Weekday
		weekday = &wd
		st, _ := hhmmToPgTime(w.StartTime) // already validated above
		et, _ := hhmmToPgTime(w.EndTime)   // already validated above
		startTime, endTime = &st, &et
	} else {
		startsAt, endsAt = w.StartsAt, w.EndsAt
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO maintenance_windows (project_id, name, weekly, starts_at, ends_at, weekday, start_time, end_time, timezone)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		w.ProjectID, w.Name, w.Weekly, startsAt, endsAt, weekday, startTime, endTime, w.Timezone,
	).Scan(&w.ID)
	if err != nil {
		return Window{}, fmt.Errorf("uptime: create window: %w", err)
	}
	return w, nil
}

// UpdateWindow rewrites a maintenance window in place.
//
// Windows were create-and-delete only, which meant that shifting a weekly
// window by an hour — the most common edit there is — cost the operator a
// retype of every field, and a one-off window whose end date moved could not be
// extended at all.
//
// project_id is part of the WHERE clause for scope: the id arrives from a form,
// and without it the owner of one project could rewrite another's window.
//
// The columns of the other schedule kind are nulled rather than left alone: a
// window switched from one-off to weekly with stale starts_at would satisfy
// neither validateWindow nor windowActive.
func (s *Service) UpdateWindow(ctx context.Context, w Window) error {
	if err := validateWindow(w); err != nil {
		return err
	}

	var startsAt, endsAt *time.Time
	var weekday *int
	var startTime, endTime *pgtype.Time
	if w.Weekly {
		wd := w.Weekday
		weekday = &wd
		st, _ := hhmmToPgTime(w.StartTime) // already validated above
		et, _ := hhmmToPgTime(w.EndTime)   // already validated above
		startTime, endTime = &st, &et
	} else {
		startsAt, endsAt = w.StartsAt, w.EndsAt
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE maintenance_windows
		SET name = $3, weekly = $4, starts_at = $5, ends_at = $6,
		    weekday = $7, start_time = $8, end_time = $9, timezone = $10
		WHERE id = $1 AND project_id = $2`,
		w.ID, w.ProjectID, w.Name, w.Weekly, startsAt, endsAt, weekday, startTime, endTime, w.Timezone,
	)
	if err != nil {
		return fmt.Errorf("uptime: update window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWindow deletes a maintenance window by id, scoped to projectID —
// same defense-in-depth as UpdateWindow's WHERE clause: the caller already
// checks windowBelongsToProject before this is reached, but a bare id would
// let one project's owner delete another project's window by a guessed id
// if that upstream check ever slipped.
func (s *Service) DeleteWindow(ctx context.Context, id, projectID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM maintenance_windows WHERE id = $1 AND project_id = $2", id, projectID)
	if err != nil {
		return fmt.Errorf("uptime: delete window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Windows returns projectID's maintenance windows.
func (s *Service) Windows(ctx context.Context, projectID int64) ([]Window, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, name, weekly, starts_at, ends_at, weekday, start_time, end_time, timezone
		FROM maintenance_windows WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: windows: %w", err)
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, fmt.Errorf("uptime: windows: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanWindow(row pgx.Row) (Window, error) {
	var w Window
	var weekday *int
	var startTime, endTime pgtype.Time
	if err := row.Scan(&w.ID, &w.ProjectID, &w.Name, &w.Weekly, &w.StartsAt, &w.EndsAt,
		&weekday, &startTime, &endTime, &w.Timezone); err != nil {
		return Window{}, err
	}
	if weekday != nil {
		w.Weekday = *weekday
	}
	w.StartTime = pgTimeToHHMM(startTime)
	w.EndTime = pgTimeToHHMM(endTime)
	return w, nil
}

// InMaintenance reports whether at falls inside any of projectID's active
// maintenance windows, honouring each window's own timezone.
func (s *Service) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	windows, err := s.Windows(ctx, projectID)
	if err != nil {
		return false, err
	}
	for _, w := range windows {
		active, err := windowActive(w, at)
		if err != nil {
			return false, fmt.Errorf("uptime: in maintenance: %w", err)
		}
		if active {
			return true, nil
		}
	}
	return false, nil
}

// WindowIntervals computes the concrete [from,to) intervals struck out by ws
// within the query range [from, to), honouring each window's own timezone
// and weekly/one-off semantics — the "exclude" argument for Query.Uptime, so
// the monitor-detail page's uptime % doesn't count checks made during
// maintenance. A window with invalid/malformed fields (should not happen —
// CreateWindow validates before persisting) is silently skipped rather than
// erroring: this helper only feeds a display computation, and one broken
// window must not blank out the whole page.
func WindowIntervals(ws []Window, from, to time.Time) []Interval {
	var out []Interval
	for _, w := range ws {
		out = append(out, windowIntervalsOne(w, from, to)...)
	}
	return out
}

// windowIntervalsOne computes w's own contribution to WindowIntervals. A
// one-off window contributes at most one interval (clipped to [from,to)); a
// weekly window contributes one interval per occurrence of its weekday that
// overlaps [from,to) — e.g. a 30-day range touches roughly four occurrences
// of a weekly window.
func windowIntervalsOne(w Window, from, to time.Time) []Interval {
	if !w.Weekly {
		if w.StartsAt == nil {
			return nil
		}
		// «Бессрочно» (EndsAt == nil): не буквальная +∞ — WindowIntervals
		// всегда зовётся с ограниченным [from,to), и клипа к `to` довольно,
		// чтобы отдать «активно до конца запрошенного диапазона» и оставить
		// caller'ам (uptime %, slo.excludeMaintenance) дело с обычными
		// конечными интервалами.
		end := to
		if w.EndsAt != nil {
			end = *w.EndsAt
		}
		iv, ok := clipInterval(*w.StartsAt, end, from, to)
		if !ok {
			return nil
		}
		return []Interval{iv}
	}

	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return nil
	}
	// Walk every calendar day (in the window's own timezone) that could
	// possibly produce an occurrence overlapping [from, to): a day before
	// from's local date can still bleed in via a midnight-crossing window,
	// so the walk starts one day before from's local date and runs through
	// one day past to's local date.
	cur := floorToDay(from.In(loc), loc).AddDate(0, 0, -1)
	end := floorToDay(to.In(loc), loc).AddDate(0, 0, 1)

	var out []Interval
	for !cur.After(end) {
		if int(cur.Weekday()) == w.Weekday {
			// Границы вхождения — из windowOccurrence, того же источника, что у
			// windowActive: два независимых вычисления одного правила
			// разъезжались в ночь перевода часов.
			start, occEnd := windowOccurrence(w, cur, loc)
			if !start.IsZero() {
				if iv, ok := clipInterval(start, occEnd, from, to); ok {
					out = append(out, iv)
				}
			}
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return out
}

func floorToDay(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// clipInterval clips [start,end) to [from,to); ok=false when the clipped
// range is empty (no overlap).
func clipInterval(start, end, from, to time.Time) (Interval, bool) {
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	if !end.After(start) {
		return Interval{}, false
	}
	return Interval{From: start, To: end}, true
}

// windowDuration — сколько окно длится по НАСТЕННЫМ часам. Плюс сутки, если
// конец не позже начала: окно переходит через полночь.
//
// Длительность существует как отдельная величина потому, что в ночь перевода
// часов пара «начало–конец» перестаёт задавать её однозначно: несуществующие
// 02:00 нормализуются в 03:00, и окно 02:00–04:00 схлопывалось до часа.
func windowDuration(w Window) time.Duration {
	sh, sm, err := parseHHMM(w.StartTime)
	if err != nil {
		return 0
	}
	eh, em, err := parseHHMM(w.EndTime)
	if err != nil {
		return 0
	}
	d := time.Duration(eh-sh)*time.Hour + time.Duration(em-sm)*time.Minute
	if d <= 0 {
		d += 24 * time.Hour
	}
	return d
}

// earliestOccurrence — первое вхождение настенного времени в этот календарный
// день.
//
// time.Date для удвоенного часа осеннего перевода возвращает вхождение ПОСЛЕ
// перевода стрелок, и окно начиналось на час позже, чем назначил оператор.
// Совпадение настенного времени двух моментов, разнесённых на час, и означает
// удвоенный час — отступаем на него назад. Одного шага довольно: переводы часов
// в базе tzdata кратны часу.
func earliestOccurrence(day time.Time, hour, minute int, loc *time.Location) time.Time {
	y, m, d := day.In(loc).Date()
	t := time.Date(y, m, d, hour, minute, 0, 0, loc)
	if prev := t.Add(-time.Hour); prev.Hour() == t.Hour() && prev.Minute() == t.Minute() {
		return prev
	}
	return t
}

// windowOccurrence — границы вхождения окна в указанный календарный день его
// пояса. Единственный источник этих границ: раньше правило перехода через
// полночь было записано отдельно в windowActive и в windowIntervalsOne, а такие
// пары разъезжаются — эта и разъехалась в ночь перевода часов.
//
// Нулевое начало означает «окно не разбирается» (битое время в конфиге);
// вызывающий такое вхождение пропускает.
func windowOccurrence(w Window, day time.Time, loc *time.Location) (time.Time, time.Time) {
	sh, sm, err := parseHHMM(w.StartTime)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	d := windowDuration(w)
	if d <= 0 {
		return time.Time{}, time.Time{}
	}
	start := earliestOccurrence(day, sh, sm, loc)
	return start, start.Add(d)
}

func windowActive(w Window, at time.Time) (bool, error) {
	if !w.Weekly {
		// Guard перед разыменованием: CHECK maintenance_windows_shape
		// гарантирует StartsAt для разовых окон, но windowActive не должен
		// зависеть от этого — защита остаётся, даже если её никогда не
		// заденет валидная строка.
		if w.StartsAt == nil {
			return false, nil
		}
		if w.EndsAt == nil {
			return !at.Before(*w.StartsAt), nil // «бессрочно»
		}
		return !at.Before(*w.StartsAt) && at.Before(*w.EndsAt), nil
	}

	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return false, err
	}
	// Проверяем вхождения сегодняшнего и вчерашнего дня: окно могло начаться
	// вчера и тянуться через полночь. Двух дней довольно — длительность окна
	// меньше суток по построению windowDuration.
	//
	// Арифметики по минутам здесь больше нет. Она и была вторым, независимым
	// изложением правила о переходе через полночь (включая prevWeekday, чьё имя
	// вычисляло СЛЕДУЮЩИЙ день), и расходилась с windowIntervalsOne в ночь
	// перевода часов.
	local := at.In(loc)
	for _, dayOffset := range []int{0, -1} {
		day := local.AddDate(0, 0, dayOffset)
		if int(day.Weekday()) != w.Weekday {
			continue
		}
		start, end := windowOccurrence(w, day, loc)
		if start.IsZero() {
			return false, fmt.Errorf("uptime: window %d: cannot resolve occurrence", w.ID)
		}
		if !at.Before(start) && at.Before(end) {
			return true, nil
		}
	}
	return false, nil
}
