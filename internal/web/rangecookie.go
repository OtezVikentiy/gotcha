package web

import (
	"net/http"
)

const rangeCookie = "range"

// В cookie попадают только пресеты TimeRangePresets: custom слишком специфичен для чужих
// страниц, "all" не годится графикам — у окна «за всё время» нет оси.
func (h *Handler) resolveTimeRange(w http.ResponseWriter, r *http.Request, def string) TimeRange {
	q := r.URL.Query()
	explicit := q.Get("period") != "" || q.Get("start") != ""
	if !explicit {
		if c, err := r.Cookie(rangeCookie); err == nil {
			if _, ok := TimeRangePresets[c.Value]; ok {
				def = c.Value
			}
		}
		return parseTimeRange(q, def)
	}
	tr := parseTimeRange(q, def)
	if _, preset := TimeRangePresets[tr.Key]; preset && !tr.Custom {
		setRangeCookie(w, tr.Key, h.Secure)
	}
	return tr
}

// Без HttpOnly — значение не секрет.
func setRangeCookie(w http.ResponseWriter, key string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     rangeCookie,
		Value:    key,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}
