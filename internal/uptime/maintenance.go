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
		return ErrInvalidWindow
	}
	if _, err := time.LoadLocation(w.Timezone); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWindow, err)
	}
	if w.Weekly {
		if w.Weekday < 0 || w.Weekday > 6 {
			return fmt.Errorf("%w: weekday must be 0..6", ErrInvalidWindow)
		}
		if _, _, err := parseHHMM(w.StartTime); err != nil {
			return fmt.Errorf("%w: invalid start_time: %v", ErrInvalidWindow, err)
		}
		if _, _, err := parseHHMM(w.EndTime); err != nil {
			return fmt.Errorf("%w: invalid end_time: %v", ErrInvalidWindow, err)
		}
		return nil
	}
	if w.StartsAt == nil || w.EndsAt == nil || !w.EndsAt.After(*w.StartsAt) {
		return fmt.Errorf("%w: one-off window needs starts_at < ends_at", ErrInvalidWindow)
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

// DeleteWindow deletes a maintenance window by id.
func (s *Service) DeleteWindow(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM maintenance_windows WHERE id = $1", id)
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
		if w.StartsAt == nil || w.EndsAt == nil {
			return nil
		}
		iv, ok := clipInterval(*w.StartsAt, *w.EndsAt, from, to)
		if !ok {
			return nil
		}
		return []Interval{iv}
	}

	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return nil
	}
	startH, startM, err := parseHHMM(w.StartTime)
	if err != nil {
		return nil
	}
	endH, endM, err := parseHHMM(w.EndTime)
	if err != nil {
		return nil
	}
	crossesMidnight := endH*60+endM <= startH*60+startM

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
			start := time.Date(cur.Year(), cur.Month(), cur.Day(), startH, startM, 0, 0, loc)
			occEnd := time.Date(cur.Year(), cur.Month(), cur.Day(), endH, endM, 0, 0, loc)
			if crossesMidnight {
				occEnd = occEnd.AddDate(0, 0, 1)
			}
			if iv, ok := clipInterval(start, occEnd, from, to); ok {
				out = append(out, iv)
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

func windowActive(w Window, at time.Time) (bool, error) {
	if !w.Weekly {
		if w.StartsAt == nil || w.EndsAt == nil {
			return false, nil
		}
		return !at.Before(*w.StartsAt) && at.Before(*w.EndsAt), nil
	}

	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return false, err
	}
	local := at.In(loc)
	startH, startM, err := parseHHMM(w.StartTime)
	if err != nil {
		return false, err
	}
	endH, endM, err := parseHHMM(w.EndTime)
	if err != nil {
		return false, err
	}
	startMinutes := startH*60 + startM
	endMinutes := endH*60 + endM
	nowMinutes := local.Hour()*60 + local.Minute()
	weekday := int(local.Weekday()) // Sunday=0..Saturday=6, matches the DB convention

	if endMinutes <= startMinutes {
		// Window spans midnight (e.g. 23:00-01:00): active either from
		// start_time to midnight on the configured weekday, or from
		// midnight to end_time on the following day.
		if weekday == w.Weekday && nowMinutes >= startMinutes {
			return true, nil
		}
		prevWeekday := (w.Weekday + 1) % 7
		return weekday == prevWeekday && nowMinutes < endMinutes, nil
	}
	return weekday == w.Weekday && nowMinutes >= startMinutes && nowMinutes < endMinutes, nil
}
