package web

import (
	"net/http"
	"strconv"
)

// projCookie — «липкость» выбранного проекта: та же идея, что у rangeCookie
// (№25). Проект в навигации задаётся только путём /projects/{id}/…, а детали
// (/issues/{id}, /traces/{id}…), /docs, /profile и область организации живут
// на адресах без проекта — там навигация молча откатывалась на ПЕРВЫЙ проект
// списка, и выбор пользователя слетал на каждом таком переходе. Cookie
// запоминает последний явно открытый проект; страницы без проекта в пути и
// корень "/" берут его отсюда.
const projCookieName = "proj"

// projCookieID — id проекта из cookie; 0 — cookie нет или значение битое.
// Доверять значению нельзя: вызывающий обязан сверить id со списком проектов,
// доступных ЭТОМУ пользователю (общий браузер, отозванный доступ).
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

// setProjCookie — та же механика, что setRangeCookie: год жизни, Path=/,
// SameSite=Lax, Secure по схеме. HttpOnly — значение читает только сервер.
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
