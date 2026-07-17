package web

import (
	"net/http"
	"net/url"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

const langCookie = "lang"

// withLocale кладёт выбранную локаль в контекст запроса. Порядок разрешения:
// cookie lang → сохранённая users.locale залогиненного (с self-heal cookie,
// см. resolveLocale) → Accept-Language → Default. /static/* пропускаем без
// резолвинга.
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

// resolveLocaleNoUser — часть цепочки без обращения к БД: cookie → Accept-Language.
// bool=true, если разрешено из cookie (тогда пользовательскую ветку пропускаем).
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
	// Нет cookie. У залогиненного берём сохранённую users.locale; если она не
	// задана — засеваем cookie разрешённым фолбэком (Accept-Language/дефолт),
	// чтобы последующие запросы шли по cookie-ветке без похода в БД (один
	// запрос к БД на сессию, а не на запрос). Анонимов НЕ засеваем: иначе
	// сохранённый язык, выбранный при будущем логине, оказался бы затенён.
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

// setLangCookie выставляет cookie lang на год. Не HttpOnly — язык не секрет, и
// клиентский код (позже) может его читать; SameSite=Lax, Secure по схеме.
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

// localeSwitch — POST /settings/locale (lang=ru|en): ставит cookie lang, для
// залогиненного пишет users.locale, редиректит на Referer (в пределах origin).
// Доступен и анониму (переключение на странице логина). sameOrigin обязателен —
// это меняющий состояние POST.
func (h *Handler) localeSwitch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.request_denied"))
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

// safeRedirect возвращает Referer, если он same-origin и его путь безопасен,
// иначе "/". Тот же паттерн, что BulkRedirectTarget: отвергает пути,
// начинающиеся с "//" (protocol-relative) или "/\" (браузер нормализует "\"
// в "/", т.е. "/\evil.com" → "//evil.com"), чтобы предотвратить открытый
// редирект.
func safeRedirect(r *http.Request, baseURL string) string {
	ref := r.Referer()
	if ref != "" && isSameOriginURL(ref, baseURL) {
		if u, err := url.Parse(ref); err == nil {
			if strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") && !strings.HasPrefix(u.Path, "/\\") {
				return u.RequestURI()
			}
		}
	}
	return "/"
}
