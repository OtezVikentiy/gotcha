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

// каждая ошибка валидации оборачивает и ErrInvalidWindow, и свою причину
// (fmt.Errorf с двумя %w) — errors.Is различает причину без парсинга текста.
var (
	ErrInvalidWindowName      = errors.New("uptime: maintenance window name required")
	ErrInvalidWindowTimezone  = errors.New("uptime: invalid maintenance window timezone")
	ErrInvalidWindowWeekday   = errors.New("uptime: invalid maintenance window weekday")
	ErrInvalidWindowStartTime = errors.New("uptime: invalid maintenance window start_time")
	ErrInvalidWindowEndTime   = errors.New("uptime: invalid maintenance window end_time")
	ErrInvalidWindowSameTime  = errors.New("uptime: maintenance window start_time and end_time must differ")
	ErrInvalidWindowRange     = errors.New("uptime: maintenance window starts_at must be before ends_at")
)

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
		// StartTime == EndTime не значит «мгновенное» окно — windowDuration
		// трактует это как полные 24 часа: опечатка молча блэкает весь день.
		if w.StartTime == w.EndTime {
			return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowSameTime)
		}
		return nil
	}
	// EndsAt == nil — окно бессрочно; StartsAt всё равно обязателен, иначе
	// не от чего вести активность (windowActive).
	if w.StartsAt == nil {
		return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowRange)
	}
	if w.EndsAt != nil && !w.EndsAt.After(*w.StartsAt) {
		return fmt.Errorf("%w: %w", ErrInvalidWindow, ErrInvalidWindowRange)
	}
	return nil
}

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

// project_id в WHERE — без него владелец одного проекта мог бы переписать
// окно другого; колонки другого расписания обнуляются, не остаются от старого.
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

// project_id в WHERE — defense-in-depth: вызывающий уже проверяет
// принадлежность окна проекту выше, но без этого условия здесь её нет вовсе.
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

// битое окно (не должно случаться — CreateWindow валидирует) молча
// пропускается, не роняя всю выборку — это только вычисление отображения.
func WindowIntervals(ws []Window, from, to time.Time) []Interval {
	var out []Interval
	for _, w := range ws {
		out = append(out, windowIntervalsOne(w, from, to)...)
	}
	return out
}

// недельное окно даёт по интервалу на каждое вхождение своего дня недели в
// [from,to), не одно на окно — 30-дневный диапазон даёт около четырёх.
func windowIntervalsOne(w Window, from, to time.Time) []Interval {
	if !w.Weekly {
		if w.StartsAt == nil {
			return nil
		}
		// EndsAt == nil клипается к `to`, не трактуется как +∞ — вызов всегда
		// идёт с ограниченным диапазоном, клипа довольно.
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
	// проход начинается на день раньше from и кончается на день позже to —
	// окно, переходящее через полночь, может зацепить эти дни.
	cur := floorToDay(from.In(loc), loc).AddDate(0, 0, -1)
	end := floorToDay(to.In(loc), loc).AddDate(0, 0, 1)

	var out []Interval
	for !cur.After(end) {
		if int(cur.Weekday()) == w.Weekday {
			// единственный источник границ вхождения — иначе легко разойтись
			// с windowActive в ночь перевода часов.
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

// ok=false, когда обрезанный интервал пуст (нет пересечения).
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

// существует отдельно от start/end: в ночь перевода часов пара не задаёт
// длительность однозначно (02:00 нормализуется в 03:00, схлопывая интервал).
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

// time.Date для удвоенного часа осеннего перевода отдаёт вхождение ПОСЛЕ
// перевода — совпадение с моментом часом раньше выявляет это и откатывает.
func earliestOccurrence(day time.Time, hour, minute int, loc *time.Location) time.Time {
	y, m, d := day.In(loc).Date()
	t := time.Date(y, m, d, hour, minute, 0, 0, loc)
	if prev := t.Add(-time.Hour); prev.Hour() == t.Hour() && prev.Minute() == t.Minute() {
		return prev
	}
	return t
}

// общий источник границ вхождения для windowActive и windowIntervalsOne;
// нулевое начало значит «не разбирается» — вызывающий его пропускает.
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
		// страховка перед разыменованием: CHECK в БД гарантирует StartsAt для
		// разовых окон, но код не должен на неё полагаться.
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
	// проверяем сегодня и вчера — окно могло начаться вчера и тянуться через
	// полночь; длительность окна короче суток по построению windowDuration.
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
