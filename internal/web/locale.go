package web

import (
	"net/http"
	"net/url"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

const langCookie = "lang"

// Порядок: cookie lang → сохранённая users.locale залогиненного → Accept-Language → Default.
// /static/* пропускаем без резолвинга.
func (h *Handler) withLocale(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		loc := h.resolveLocale(w, r)
		next.ServeHTTP(w, r.WithContext(i18n.WithLocale(r.Context(), loc)))
	})
}

// bool=true, если разрешено из cookie — тогда пользовательскую ветку пропускаем.
func resolveLocaleNoUser(r *http.Request) (i18n.Locale, bool) {
	if c, err := r.Cookie(langCookie); err == nil {
		if loc, ok := i18n.Parse(c.Value); ok {
			return loc, true
		}
	}
	return i18n.Match(r.Header.Get("Accept-Language")), false
}

func (h *Handler) resolveLocale(w http.ResponseWriter, r *http.Request) i18n.Locale {
	loc, fromCookie := resolveLocaleNoUser(r)
	if fromCookie {
		return loc
	}
	// Нет cookie: у залогиненного берём users.locale, иначе засеваем cookie фолбэком — один
	// запрос к БД на сессию. Анонимов не засеваем: затенило бы язык, выбранный при логине.
	if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
		if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
			if code, err := h.Auth.UserLocale(r.Context(), uid); err == nil {
				if ul, ok := i18n.Parse(code); ok {
					setLangCookie(w, ul.Code, h.Secure)
					return ul
				}
				setLangCookie(w, loc.Code, h.Secure)
			}
		}
	}
	return loc
}

// Не HttpOnly: язык не секрет, клиентский код может его читать.
func setLangCookie(w http.ResponseWriter, code string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    code,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

func (h *Handler) localeSwitch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	loc, ok := i18n.Parse(r.FormValue("lang"))
	if ok {
		setLangCookie(w, loc.Code, h.Secure)
		if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
			if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
				_ = h.Auth.SetLocale(r.Context(), uid, loc.Code)
			}
		}
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}

// Управляющие символы (TAB и т.п.) браузер выбрасывает из адреса до разбора URL —
// «/<TAB>/evil.example» доезжает как «//evil.example», обходя проверки "//" и "/\" без этого.
func isLocalPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return false
	}
	return strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// Повторяет проверку safeNextPath намеренно: решение «адрес свой» принимается там, где
// ставится Location, а не остаётся обещанием другой функции.
func redirectLocal(w http.ResponseWriter, r *http.Request, dest string) {
	if !isLocalPath(dest) {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// Пустая строка — «веди на главную». Значение от клиента, проверяется теми же правилами,
// что Referer в safeRedirect: только относительный путь этого же сайта, без схемы и хоста.
func safeNextPath(raw string) string {
	if raw == "" || !isLocalPath(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return u.RequestURI()
}

func safeRedirect(r *http.Request, baseURL string) string {
	ref := r.Referer()
	if ref != "" && isSameOriginURL(ref, baseURL) {
		if u, err := url.Parse(ref); err == nil {
			if isLocalPath(u.Path) {
				return u.RequestURI()
			}
		}
	}
	return "/"
}
