package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func resetPasswordPath(token string) string {
	return "/reset-password/" + token
}

func (h *Handler) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.ForgotPassword("", false, h.EmailEnabled, "").Render(r.Context(), w)
}

func (h *Handler) forgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	// Инстанс без почты не может доставить ссылку — форма не принимается даже прямым
	// POST мимо скрытой на GET формы.
	if !h.EmailEnabled {
		_ = templates.ForgotPassword("", false, false, "").Render(r.Context(), w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := normalizeEmail(r.FormValue("email"))

	// СВОИ лимитеры, не общие с login/register: общий ключ по email запирал бы жертву на
	// собственном входе потоком восстановлений с чужих IP. IP первым — как в loginSubmit.
	if !h.passwordResetIPLimiter.Allow(h.clientIP(r)) || !h.passwordResetEmailLimiter.Allow(limiterEmailKeyPart(email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.ForgotPassword(i18n.T(r.Context(), "err.auth.rate_limited"), false, true, email).Render(r.Context(), w)
		return
	}

	// Ответ уходит ДО поиска пользователя: токен, локаль и письмо зависят от того, есть ли
	// такой адрес, и вместе давали различимый тайминг даже с фоновой отправкой SMTP.
	go h.processPasswordResetRequest(email)
	_ = templates.ForgotPassword("", true, true, "").Render(r.Context(), w)
}

const passwordResetLookupTimeout = 10 * time.Second

// Фон, вне пути ответа: собственный ctx, не r.Context() — тот к этому моменту уже завершится.
// Ошибка RequestPasswordReset только логируется — синхронного ответа ей адресовать уже некуда.
func (h *Handler) processPasswordResetRequest(email string) {
	ctx, cancel := context.WithTimeout(context.Background(), passwordResetLookupTimeout)
	defer cancel()
	token, found, err := h.Auth.RequestPasswordReset(ctx, email)
	if err != nil {
		slog.Error("forgotPasswordSubmit: request reset failed", "error", err)
		return
	}
	if !found {
		return
	}
	ctx = i18n.WithLocale(ctx, h.recipientLocale(ctx, email, h.NotifyLocale))
	subject := i18n.T(ctx, "auth.forgot.email_subject")
	body := i18n.Tf(ctx, "auth.forgot.email_body", "link", h.BaseURL+resetPasswordPath(token))
	h.deliverPasswordResetEmail(email, subject, body)
}

// Личное письмо получателю, не канальное уведомление — читает users.locale адресата,
// а не запроса отправителя. fallback — для незарегистрированного адреса или без явного выбора.
func (h *Handler) recipientLocale(ctx context.Context, email string, fallback i18n.Locale) i18n.Locale {
	uid, err := h.Auth.UserByEmail(ctx, email)
	if err != nil {
		return fallback
	}
	code, err := h.Auth.UserLocale(ctx, uid)
	if err != nil {
		return fallback
	}
	if l, ok := i18n.Parse(code); ok {
		return l
	}
	return fallback
}

const passwordResetEmailTimeout = 30 * time.Second

func (h *Handler) deliverPasswordResetEmail(email, subject, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), passwordResetEmailTimeout)
	defer cancel()
	if h.Email == nil || !h.Email.Configured() {
		return
	}
	if err := h.Email.Send(ctx, notify.Target{Kind: "email", Target: email}, map[string]any{
		"subject": subject, "body": body,
	}); err != nil {
		// err (обёрнутый wrapSMTPErr в notify) не несёт ни ссылку, ни токен — журналировать
		// в этой ошибке нечего, дальше передавать некуда.
		slog.Warn("forgotPasswordSubmit: failed to send reset email", "error", err)
	}
}

func (h *Handler) resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !h.Auth.ValidPasswordResetToken(r.Context(), token) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.ResetPassword(token, i18n.T(r.Context(), "err.auth.reset_invalid"), false).Render(r.Context(), w)
		return
	}
	_ = templates.ResetPassword(token, "", true).Render(r.Context(), w)
}

func (h *Handler) resetPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	token := r.PathValue("token")
	// Токен не перебираем (256 бит энтропии) — лимит здесь ради единообразия с соседними
	// ручками и чтобы шум не доходил до БД, а не как защита от подбора.
	if !h.passwordResetIPLimiter.Allow(h.clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.ResetPassword(token, i18n.T(r.Context(), "err.auth.rate_limited"), true).Render(r.Context(), w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	newPassword := r.FormValue("new")
	newPassword2 := r.FormValue("new2")

	if newPassword != newPassword2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.ResetPassword(token, i18n.T(r.Context(), "err.auth.passwords_differ"), true).Render(r.Context(), w)
		return
	}

	switch err := h.Auth.ResetPassword(r.Context(), token, newPassword); {
	case err == nil:
		h.flashOK(w, "flash.password_reset", 0)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case errors.Is(err, auth.ErrResetTokenInvalid):
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.ResetPassword(token, i18n.T(r.Context(), "err.auth.reset_invalid"), false).Render(r.Context(), w)
	case errors.Is(err, auth.ErrWeakPassword):
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.ResetPassword(token, i18n.T(r.Context(), "err.profile.password_length"), true).Render(r.Context(), w)
	default:
		h.renderError(w, r, http.StatusInternalServerError, "")
	}
}
