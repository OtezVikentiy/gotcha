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
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
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
	next := safeNextPath(r.URL.Query().Get("next"))
	// PROD-B1: если режим не open и первый пользователь уже есть — показываем
	// экран «регистрация по приглашению» вместо формы (bootstrap уже пройден).
	if h.registrationClosed(r) {
		_ = templates.Register(i18n.T(r.Context(), "error.register.closed"), true, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}
	_ = templates.Register("", false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
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

// inviteTokenFromNext — токен приглашения из адреса, куда человек вернётся
// после регистрации.
//
// Это единственное доказательство права на регистрацию в режиме invite.
// Знание приглашённого адреса им не является: раньше являлось, и по нему
// аноним заводил аккаунт в чужой организации с ролью приглашения.
//
// Разбор строгий: ровно /invite/{token}, без лишних сегментов и без query —
// адрес пришёл из формы, и вольность в его разборе стала бы новой поверхностью.
func inviteTokenFromNext(next string) (string, bool) {
	const prefix = "/invite/"
	if !strings.HasPrefix(next, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(next, prefix)
	if token == "" || strings.ContainsAny(token, "/?#") {
		return "", false
	}
	return token, true
}

// invitedByToken — пускать ли эту регистрацию. Сам отвечает отказом, если нет:
// решение и ответ живут рядом, чтобы вызывающий не мог забыть один из исходов.
//
// Требуется И живой токен, И совпадение адреса с адресом приглашения. Одного
// токена мало: утёкшая ссылка иначе позволила бы завести аккаунт на закрытом
// инстансе под произвольным адресом — членства он бы не дал, но squatter в
// базе появился бы, а к моменту AcceptInvite аккаунт уже создан.
//
// Приглашение здесь только читается, но не гасится: гасит его AcceptInvite по
// тому же токену, уже после входа. Потратить приглашение раньше, чем человек
// им воспользуется, значило бы оставить его с аккаунтом, но без организации.
//
// Все причины отказа выглядят для клиента одинаково: различие между ними и
// было оракулом, по которому перебором проверяли, кто приглашён.
func (h *Handler) invitedByToken(w http.ResponseWriter, r *http.Request, next, email string) bool {
	// closed — новых аккаунтов не появляется вообще, даже по действующему
	// приглашению; ровно этим он и отличается от invite (та же граница, что и в
	// oauthProvisionByInvite).
	if h.RegistrationMode != "invite" || h.Org == nil {
		h.denyRegistration(w, r, next, "mode_closed")
		return false
	}
	token, ok := inviteTokenFromNext(next)
	if !ok {
		h.denyRegistration(w, r, next, "no_token")
		return false
	}
	inv, err := h.Org.InviteByToken(r.Context(), token)
	if errors.Is(err, org.ErrInviteInvalid) {
		h.denyRegistration(w, r, next, "bad_token")
		return false
	}
	if err != nil {
		// Fail closed: не знаем, действительно ли приглашение — не заводим аккаунт.
		slog.Error("register: invite lookup failed", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return false
	}
	// Регистр адреса закрывает сама колонка (org_invites.email — citext), но
	// сравнение здесь своё: значение пришло из формы, а решение принимается тут.
	if !strings.EqualFold(inv.Email, email) {
		h.denyRegistration(w, r, next, "email_mismatch")
		return false
	}
	return true
}

// denyRegistration отказывает в регистрации и рисует ту же заглушку, что и
// закрытый режим (registrationClosed). next передаётся в шаблон, чтобы
// адресат не терялся при отказе — человек всё ещё может уйти на /login по
// той же ссылке-приглашению.
//
// reason уходит только в лог: клиенту все причины отказа обязаны выглядеть
// одинаково, иначе различие снова становится оракулом. Адрес в лог не пишется
// — это ПДн, а для разбора хватает причины и IP.
func (h *Handler) denyRegistration(w http.ResponseWriter, r *http.Request, next, reason string) {
	slog.Warn("register: denied", "reason", reason, "ip", h.clientIP(r))
	w.WriteHeader(http.StatusForbidden)
	_ = templates.Register(i18n.T(r.Context(), "auth.register.invite_required"), true, next,
		h.oauthButtons(r.Context())).Render(r.Context(), w)
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
	next := safeNextPath(r.FormValue("next"))

	// SEC-L2: per-account (ip|email), глобальный per-IP и per-email (без IP) —
	// тот же набор и тот же порядок, что в loginSubmit. Per-email нужен и здесь:
	// без него распределённый перебор одного приглашённого адреса с пула IP не
	// ограничивался ничем.
	if !h.loginLimiter.Allow(h.rateLimitKey(r, email)) || !h.ipLimiter.Allow(h.clientIP(r)) ||
		!h.emailLimiter.Allow(normalizeEmail(email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.rate_limited_register"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// PROD-B1: гейтинг регистрации по режиму. Первый пользователь инстанса
	// всегда может зарегистрироваться (bootstrap инстанс-админа); дальше — по
	// режиму.
	//
	//   open   — всегда открыто;
	//   invite — только по токену из ссылки-приглашения, выписанной на этот адрес;
	//   closed — только bootstrap первого.
	//
	// Раньше в режиме invite хватало совпадения введённого адреса с адресом
	// действующего приглашения (P0 №2 аудита 2026-07-30): подтверждения
	// владения адресом не было нигде, и аноним, знающий приглашённый адрес,
	// получал аккаунт и — сразу же, ниже по этой функции — членство в чужой
	// организации с ролью приглашения. Теперь правом служит только токен, см.
	// invitedByToken.
	if h.RegistrationMode != "open" {
		n, err := h.Auth.UserCount(r.Context())
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		// Адрес нормализуем так же, как Auth.Register и форма приглашения:
		// обрамляющие пробелы Register отрежет и заведёт аккаунт, а сравнение с
		// адресом приглашения по строке с пробелами не совпало бы, и человек
		// получил бы отказ при живом приглашении.
		if n > 0 && !h.invitedByToken(w, r, next, normalizeEmail(email)) {
			return
		}
	}

	if password != password2 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(i18n.T(r.Context(), "err.auth.passwords_differ"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
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
		_ = templates.Register(i18n.T(r.Context(), "err.auth.sso_required"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Register(registerErrorMessage(r.Context(), err), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	// Членство здесь НЕ выдаётся. Раньше выдавалось — по совпадению введённого
	// адреса с адресом приглашения, то есть любому, кто этот адрес знал.
	// Приглашение гасит только AcceptInvite, по токену и уже после входа: там
	// человек видит, в какую организацию его зовут, и соглашается явно.
	// Регистрация же — лишь пропуск к этой форме, и адресат ведёт ровно туда
	// (redirectLocal ниже), так что приглашённый попадает на неё сразу.
	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	// Возврат туда, куда человек шёл до формы регистрации — та же логика, что
	// и в loginSubmit (см. комментарий там): без этого ссылка-приглашение
	// теряется после регистрации, а не только после входа.
	redirectLocal(w, r, next)
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
