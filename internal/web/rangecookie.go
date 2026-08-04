package web

import (
	"net/http"
)

// rangeCookie — «липкость» выбранного окна времени (№25): явный выбор
// пресета запоминается и становится дефолтом на остальных страницах, ссылки
// навигации при этом остаются чистыми (без query).
const rangeCookie = "range"

// resolveTimeRange — единственная точка входа хендлеров к окну времени.
// Резолв: явный query важнее cookie и записывает выбор; без query берётся
// cookie; без обоих — дефолт страницы.
//
// В cookie попадают ТОЛЬКО пресеты из TimeRangePresets: ни custom-диапазоны
// (слишком специфичны, чтобы навязывать их другим страницам), ни "all"
// (родной дефолт списков; графикам как навязанный дефолт не годится — у окна
// «за всё время» нет оси). Невалидный cookie молча игнорируется — страница
// живёт на своём дефолте.
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

// setRangeCookie — та же механика, что setThemeCookie: год жизни, не
// HttpOnly (не секрет), SameSite=Lax, Secure по схеме.
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
