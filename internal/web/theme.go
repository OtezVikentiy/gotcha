package web

import (
	"net/http"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

const themeCookie = "theme"

// withTheme кладёт выбранную тему оформления в контекст запроса. Порядок
// разрешения: cookie theme → сохранённая users.theme залогиненного (с
// self-heal cookie, см. resolveTheme) → Default (system). /static/*
// пропускаем без резолвинга.
func (h *Handler) withTheme(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		t := h.resolveTheme(w, r)
		next.ServeHTTP(w, r.WithContext(theme.WithTheme(r.Context(), t)))
	})
}

// resolveThemeNoUser — часть цепочки без обращения к БД: cookie → Default.
// bool=true, если разрешено из cookie (тогда пользовательскую ветку пропускаем).
func resolveThemeNoUser(r *http.Request) (theme.Theme, bool) {
	if c, err := r.Cookie(themeCookie); err == nil {
		if t, ok := theme.Parse(c.Value); ok {
			return t, true
		}
	}
	return theme.Default, false
}

func (h *Handler) resolveTheme(w http.ResponseWriter, r *http.Request) theme.Theme {
	t, fromCookie := resolveThemeNoUser(r)
	if fromCookie {
		return t
	}
	// Нет cookie. У залогиненного берём сохранённую users.theme; если она не
	// задана — засеваем cookie разрешённым фолбэком (Default), чтобы
	// последующие запросы шли по cookie-ветке без похода в БД (один запрос к
	// БД на сессию, а не на запрос). Анонимов НЕ засеваем: иначе сохранённая
	// тема, выбранная при будущем логине, оказалась бы затенена.
	if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
		if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
			if code, err := h.Auth.UserTheme(r.Context(), uid); err == nil {
				if ut, ok := theme.Parse(code); ok {
					setThemeCookie(w, ut.Code, h.Secure)
					return ut
				}
				setThemeCookie(w, t.Code, h.Secure)
			}
		}
	}
	return t
}

// setThemeCookie выставляет cookie theme на год. Не HttpOnly — тема не
// секрет, и клиентский код (позже) может её читать; SameSite=Lax, Secure по
// схеме.
func setThemeCookie(w http.ResponseWriter, code string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookie,
		Value:    code,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// themeSwitch — POST /settings/theme (theme=dark|light|system): ставит
// cookie theme, для залогиненного пишет users.theme, редиректит на Referer
// (в пределах origin). Доступен и анониму. sameOrigin обязателен — это
// меняющий состояние POST.
func (h *Handler) themeSwitch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.request_denied"))
		return
	}
	t, ok := theme.Parse(r.FormValue("theme"))
	if ok {
		setThemeCookie(w, t.Code, h.Secure)
		if tok, ok := auth.ReadSessionToken(r, h.Secure); ok {
			if uid, err := h.Auth.SessionUser(r.Context(), tok); err == nil {
				_ = h.Auth.SetTheme(r.Context(), uid, t.Code)
			}
		}
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}
