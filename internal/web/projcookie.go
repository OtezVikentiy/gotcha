package web

import (
	"net/http"
	"strconv"
)

const projCookieName = "proj"

// Значению нельзя доверять: id нужно сверить со списком проектов, доступных ЭТОМУ пользователю.
func projCookieID(r *http.Request) int64 {
	c, err := r.Cookie(projCookieName)
	if err != nil {
		return 0
	}
	id, err := strconv.ParseInt(c.Value, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func setProjCookie(w http.ResponseWriter, id int64, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     projCookieName,
		Value:    strconv.FormatInt(id, 10),
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}
