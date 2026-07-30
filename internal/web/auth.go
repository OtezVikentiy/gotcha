package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// oauthButtons собирает кнопки включённых провайдеров для страниц входа.
func (h *Handler) oauthButtons(ctx context.Context) []templates.OAuthButton {
	if h.OAuth == nil {
		return nil
	}
	var out []templates.OAuthButton
	for _, p := range h.OAuth.List() {
		out = append(out, templates.OAuthButton{Name: p.Name(), Label: i18n.Tf(ctx, "auth.oauth.login_with", "provider", p.DisplayName())})
	}
	return out
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.Login("", safeNextPath(r.URL.Query().Get("next")), h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	// PROD-B1: если режим не open и первый пользователь уже есть — показываем
	// экран «регистрация по приглашению» вместо формы (bootstrap уже пройден).
	if h.registrationClosed(r) {
		_ = templates.Register(i18n.T(r.Context(), "error.register.closed"), true, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}
	_ = templates.Register("", false, h.oauthButtons(r.Context())).Render(r.Context(), w)
}

// registrationClosed сообщает, надо ли вместо формы регистрации показать
// заглушку «регистрация закрыта».
//
// В режиме invite форма ПОКАЗЫВАЕТСЯ: человек приходит по ссылке-приглашению и
// должен завести аккаунт, а есть ли на его адрес действующее приглашение,
// известно только после ввода адреса. Отказ выдаётся уже на отправку
// (registerSubmit) — иначе приглашённому просто некуда вводить свой email.
//
// closed прячет форму сразу: там новых аккаунтов не появляется вовсе.
// Ошибку подсчёта трактуем как «не закрыто», чтобы не прятать форму из-за
// временного сбоя БД — фактический гейтинг всё равно в registerSubmit.
func (h *Handler) registrationClosed(r *http.Request) bool {
	if h.RegistrationMode != "closed" {
		return false
	}
	n, err := h.Auth.UserCount(r.Context())
	if err != nil {
		return false
	}
	return n > 0
}

// normalizeEmail — адрес в том виде, в котором он хранится: нижний регистр без
// обрамляющих пробелов (та же нормализация, что в auth.Service.Register и в
// форме приглашения).
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// invitedByEmail — пускать ли этот адрес на регистрацию по приглашению. Сам
// отвечает отказом (403 со страницей регистрации), если нет: решение и ответ
// живут рядом, чтобы вызывающий не мог забыть один из двух исходов.
//
// Приглашение здесь только проверяется, но не гасится: гасит его AcceptInvite
// по токену из ссылки, уже после входа. Регистрация — это лишь пропуск к форме
// приглашения, и потратить приглашение раньше, чем человек им воспользуется,
// означало бы оставить его с аккаунтом, но без организации.
func (h *Handler) invitedByEmail(w http.ResponseWriter, r *http.Request, email string) bool {
	// closed — новых аккаунтов не появляется вообще, даже по действующему
	// приглашению; ровно этим он и отличается от invite (та же граница, что и в
	// oauthProvisionByInvite).
	if h.RegistrationMode != "invite" || h.Org == nil {
		h.denyRegistration(w, r)
		return false
	}
	has, err := h.Org.HasPendingInvite(r.Context(), email)
	if err != nil {
		// Fail closed: не знаем, приглашён ли — не заводим аккаунт.
		slog.Error("register: pending invite lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return false
	}
	if !has {
		h.denyRegistration(w, r)
		return false
	}
	return true
}

func (h *Handler) denyRegistration(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusForbidden)
	_ = templates.Register(i18n.T(r.Context(), "error.register.closed"), true, h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	// SEC-L2: сначала per-account (ip|email), затем глобальный per-IP лимит, затем
	// per-email (без IP) — против распределённого перебора одного аккаунта с пула
	// IP. Любое превышение → 429. Порядок важен: короткое замыкание || не
	// расходует последующие бакеты, если ранний уже отказал.
	emailKey := strings.ToLower(strings.TrimSpace(email))
	if !h.loginLimiter.Allow(h.rateLimitKey(r, email)) || !h.ipLimiter.Allow(h.clientIP(r)) ||
		!h.emailLimiter.Allow(emailKey) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.rate_limited_login"), safeNextPath(r.FormValue("next")), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// Принуждение SSO (этап 10, SEC-H2): если домен email принадлежит организации с
	// enforced-SSO, пароль не принимаем — только вход через SSO.
	enforced, err := h.enforcedSSO(r.Context(), emailDomain(email))
	if err != nil {
		// Fail closed: неизвестно, обязателен ли SSO для домена — не пускаем.
		slog.Error("login: enforced SSO lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if enforced {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.sso_required"), safeNextPath(r.FormValue("next")), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.bad_credentials"), safeNextPath(r.FormValue("next")), h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	// Возврат туда, куда человек шёл до формы входа (см. safeNextPath и
	// auth.loginWithNext): иначе глубокая ссылка — приглашение, ссылка на
	// проблему из письма алерта — теряется, и он оказывается на главной.
	redirectLocal(w, r, safeNextPath(r.FormValue("next")))
}

func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")

	// SEC-L2: per-account (ip|email) + глобальный per-IP лимит, см. loginSubmit.
	if !h.loginLimiter.Allow(h.rateLimitKey(r, email)) || !h.ipLimiter.Allow(h.clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.rate_limited_register"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// PROD-B1: гейтинг регистрации по режиму. Первый пользователь инстанса
	// всегда может зарегистрироваться (bootstrap инстанс-админа); дальше — по
	// режиму.
	//
	//   open   — всегда открыто;
	//   invite — только если на этот адрес есть действующее приглашение;
	//   closed — только bootstrap первого.
	//
	// Проверка приглашения раньше жила ТОЛЬКО в OAuth-ветке
	// (oauthProvisionByInvite), а парольная регистрация при любом не-open
	// режиме отдавала 403. То есть в режиме invite — а он по умолчанию —
	// приглашённый мог войти лишь через OAuth-провайдера, и на типовой
	// self-hosted-инсталляции без такого провайдера ссылка-приглашение не
	// работала вовсе: человек шёл по ней, его отправляло на регистрацию, и там
	// он получал «регистрация закрыта». Документация при этом обещала ровно
	// обратное.
	if h.RegistrationMode != "open" {
		n, err := h.Auth.UserCount(r.Context())
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		// Адрес нормализуем так же, как Auth.Register и форма приглашения.
		// Регистр закрывает сама колонка (org_invites.email — citext), а вот
		// обрамляющие пробелы — нет: Register их отрежет и заведёт аккаунт, а
		// поиск приглашения по строке с пробелами ничего не найдёт, и человек
		// получит «регистрация закрыта» при живом приглашении.
		if n > 0 && !h.invitedByEmail(w, r, normalizeEmail(email)) {
			return
		}
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.passwords_differ"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// SEC-H2: домен с enforced-SSO не может регистрироваться паролем (обход
	// централизованного provisioning/деprovisioning). Как в loginSubmit.
	enforced, err := h.enforcedSSO(r.Context(), emailDomain(email))
	if err != nil {
		// Fail closed: домен может требовать SSO — регистрацию паролем не даём.
		slog.Error("register: enforced SSO lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if enforced {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.sso_required"), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(registerErrorMessage(r.Context(), err), false, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// Приглашение гасится сразу после регистрации — так же, как это делает
	// OAuth-ветка (oauthProvisionByInvite).
	//
	// Без этого приглашённый заводил аккаунт и оказывался НИ В ОДНОЙ
	// организации: главная отправляла его в онбординг «создайте организацию и
	// первый проект», хотя он пришёл по ссылке в чужую. Выбраться можно было
	// только вернувшись в почту и открыв ссылку заново — и то лишь потому, что
	// к тому моменту он уже залогинен.
	//
	// Ошибка принятия не отменяет регистрацию: аккаунт уже создан и им можно
	// пользоваться, а приглашение остаётся действующим — человек примет его по
	// ссылке из письма. Откатывать пользователя, как это делает OAuth-ветка,
	// здесь нельзя: там аккаунт заводится ТОЛЬКО ради приглашения, а тут
	// регистрация самостоятельна.
	if h.Org != nil {
		if _, ok, err := h.Org.AcceptPendingInviteByEmail(r.Context(), normalizeEmail(email), uid); err != nil {
			slog.Error("register: accepting pending invite failed", "user_id", uid, "error", err)
		} else if !ok {
			slog.Info("register: no pending invite to accept", "user_id", uid)
		}
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func registerErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		// SEC-L1: не раскрываем существование аккаунта (enumeration) —
		// нейтральная формулировка вместо «этот email уже зарегистрирован».
		return i18n.T(ctx, "error.register.email_taken")
	case errors.Is(err, auth.ErrWeakPassword):
		return i18n.T(ctx, "error.register.weak_password")
	case errors.Is(err, auth.ErrInvalidEmail):
		return i18n.T(ctx, "error.register.invalid_email")
	default:
		return i18n.T(ctx, "error.register.failed")
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if token, ok := auth.ReadSessionToken(r, h.Secure); ok {
		_ = h.Auth.DestroySession(r.Context(), token)
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ssoPage — GET /sso: identifier-first вход (этап 10). Поле email → по домену
// резолвим SSO организации.
func (h *Handler) ssoPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.SSOLogin("").Render(r.Context(), w)
}

// ssoSubmit — POST /sso: резолв org_sso по email-домену → редирект на SSO-start
// организации. Неизвестный домен → нейтральное сообщение (не палим список доменов).
func (h *Handler) ssoSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	if !h.loginLimiter.Allow("sso|" + h.rateLimitKey(r, email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.SSOLogin(i18n.T(r.Context(), "err.auth.rate_limited")).Render(r.Context(), w)
		return
	}
	cfg, ok, err := h.Org.SSOByDomain(r.Context(), emailDomain(email))
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.SSOLogin(i18n.T(r.Context(), "err.auth.sso_not_configured")).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/auth/oauth/"+ssoProviderPrefix+strconv.FormatInt(cfg.OrgID, 10)+"/start", http.StatusSeeOther)
}
