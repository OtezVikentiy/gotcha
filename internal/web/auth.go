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

// authFormMaxBodyBytes — потолок тела POST для форм логина, регистрации и
// выбора SSO. Полям формы (email, пароль, next) с большим запасом хватает
// нескольких сотен байт; значение взято на порядки ниже дефолтного потолка
// net/http на форму (10 МиБ) — без явного лимита это тело доходит до
// rateLimitKey (см. ratelimit.go) целиком, каким бы большим оно ни было.
const authFormMaxBodyBytes = 8 << 10 // 8 KiB

// providerLabel — подпись OAuth-провайдера по локали зрителя (№137): у
// известных провайдеров ключ oauth.provider.<name> в каталоге, у generic
// OIDC с произвольным именем из конфига ключа нет (T возвращает сам ключ) —
// тогда DisplayName как есть.
func providerLabel(ctx context.Context, name, displayName string) string {
	key := "oauth.provider." + name
	if s := i18n.T(ctx, key); s != key {
		return s
	}
	return displayName
}

// oauthButtons собирает кнопки включённых провайдеров для страниц входа.
func (h *Handler) oauthButtons(ctx context.Context) []templates.OAuthButton {
	if h.OAuth == nil {
		return nil
	}
	var out []templates.OAuthButton
	for _, p := range h.OAuth.List() {
		out = append(out, templates.OAuthButton{Name: p.Name(), Label: i18n.Tf(ctx, "auth.oauth.login_with", "provider", providerLabel(ctx, p.Name(), p.DisplayName()))})
	}
	return out
}

// resolveAuthNext — адресат для GET /login и /register.
//
// Сперва query next — общий механизм глубоких ссылок (issue из письма
// алерта и т.п., см. auth.loginWithNext в internal/auth/middleware.go): он не
// секрет, ему в query самое место. Если его нет — invite-cookie (K9-19):
// ссылки со страницы приглашения ведут на /login и /register БЕЗ next в
// query, а куда вернуться после входа, помнит cookie, выставленная
// inviteAcceptPage (см. invitecookie.go).
//
// Легаси-случай — next в query уже содержит путь приглашения (старая ссылка,
// сохранённая до этого исправления, либо адрес набран руками): такой next
// по-прежнему возвращается как есть (иначе ссылка перестала бы работать), но
// токен из него сразу же зеркалится в cookie — иначе следующая ссылка
// «войти» на RegisterStub (см. denyRegistration и loginLinkWithNext)
// повторила бы ту же утечку в свою очередь.
func (h *Handler) resolveAuthNext(w http.ResponseWriter, r *http.Request, rawNext string) string {
	next := safeNextPath(rawNext)
	if next != "" {
		if token, ok := inviteTokenFromNext(next); ok {
			h.setInviteNextCookie(w, token)
		}
		return next
	}
	if token, ok := inviteNextToken(r); ok {
		return inviteAcceptPath(token)
	}
	return ""
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	next := h.resolveAuthNext(w, r, r.URL.Query().Get("next"))
	_ = templates.Login("", next, "", h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	next := h.resolveAuthNext(w, r, r.URL.Query().Get("next"))
	// PROD-B1: если режим closed и первый пользователь уже есть — показываем
	// экран «регистрация закрыта» вместо формы (bootstrap уже пройден).
	if h.registrationClosed(r) {
		// Без errMsg (№68): на GET закрытая регистрация — штатное состояние,
		// о нём рассказывает информационный абзац шаблона; красная плашка
		// остаётся реальным отказам POST (см. denyRegistration).
		_ = templates.RegisterStub("", h.RegistrationMode, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}
	_ = templates.RegisterForm("", h.inviteOnlyNotice(r, next), next, h.oauthButtons(r.Context())).Render(r.Context(), w)
}

// inviteOnlyNotice — показывать ли на форме регистрации предупреждение «только
// по приглашению» (QA MINOR-UX-2): режим invite, bootstrap уже пройден, а
// токена приглашения в next нет. Сабмит такой формы упрётся в 403, и честнее
// сказать об этом до ввода пароля, а не после. С токеном (человек пришёл по
// ссылке-приглашению) предупреждение не показывается — его путь штатный.
func (h *Handler) inviteOnlyNotice(r *http.Request, next string) bool {
	if h.RegistrationMode != "invite" {
		return false
	}
	if _, ok := inviteTokenFromNext(next); ok {
		return false
	}
	n, err := h.Auth.UserCount(r.Context())
	if err != nil {
		// Ошибка подсчёта — предупреждение показываем: оно информационное, и в
		// устоявшемся invite-инстансе правдиво почти всегда; гейтинг сабмита
		// от него не зависит (registerSubmit решает сам).
		return true
	}
	return n > 0
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
	// oauthProvision).
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

// denyRegistration отказывает в регистрации и рисует заглушку без формы
// (RegisterStub). next передаётся в шаблон, чтобы адресат не терялся при
// отказе — человек всё ещё может уйти на /login по той же
// ссылке-приглашению.
//
// reason уходит только в лог: ВНУТРИ режима invite все причины отказа
// (нет токена/битый токен/чужой адрес) обязаны выглядеть одинаково, иначе
// различие снова становится оракулом. Между режимами копия честно разная
// (QA MINOR-UX-2): сам режим — не секрет, его показывает уже GET, а совет
// «получите приглашение» в closed-режиме вводил в заблуждение — там оно не
// помогает. Адрес в лог не пишется — это ПДн, для разбора хватает причины
// и IP.
//
// Если next всё ещё несёт токен приглашения (пришёл со скрытого поля формы,
// см. RegisterForm) — перевзводим invite-cookie тем же токеном. Она нужна на
// следующем шаге: ссылка «войти» на RegisterStub не кладёт токен в query
// (loginLinkWithNext, K9-19) и опирается только на куку, а её TTL
// (inviteNextTTL, 10 минут) мог истечь за время, пока человек заполнял
// форму, — без перевзвода ссылка вела бы в никуда, и вернуться можно было бы
// только по письму заново.
func (h *Handler) denyRegistration(w http.ResponseWriter, r *http.Request, next, reason string) {
	slog.Warn("register: denied", "reason", reason, "ip", h.clientIP(r))
	if token, ok := inviteTokenFromNext(next); ok {
		h.setInviteNextCookie(w, token)
	}
	msg := i18n.T(r.Context(), "auth.register.invite_required")
	if h.RegistrationMode == "closed" {
		msg = i18n.T(r.Context(), "auth.register.closed_denied")
	}
	w.WriteHeader(http.StatusForbidden)
	_ = templates.RegisterStub(msg, h.RegistrationMode, next,
		h.oauthButtons(r.Context())).Render(r.Context(), w)
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	// SEC-L2: сначала глобальный per-IP лимит (дешёвый по кардинальности —
	// один ключ на IP), затем per-account (ip|email) и per-email (без IP) —
	// против распределённого перебора одного аккаунта с пула IP. Любое
	// превышение → 429.
	//
	// Порядок важен и это не просто оптимизация (находка W2-B, рабочий
	// эксплойт): ip|email — самый дорогой по кардинальности ключ, её задаёт
	// атакующий подстановкой произвольного email, — раньше проверялся
	// ПЕРВЫМ операндом ||, то есть ДО per-IP лимита. Из-за этого один IP с
	// потоком выдуманных email мог заполнить карту loginLimiter (у неё есть
	// потолок числа ключей, см. web.go) быстрее, чем успевал сработать
	// ipLimiter, — после чего КАЖДЫЙ новый легитимный пользователь получал
	// отказ по переполнению карты. Короткое замыкание || теперь работает на
	// нас: отказавший дешёвый ipLimiter не даёт дорогим лимитерам завести
	// новую запись вовсе.
	emailKey := limiterEmailKeyPart(email)
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow(h.rateLimitKey(r, email)) ||
		!h.emailLimiter.Allow(emailKey) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.rate_limited_login"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
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
		_ = templates.Login(i18n.T(r.Context(), "err.auth.sso_required"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Authenticate(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Login(i18n.T(r.Context(), "err.auth.bad_credentials"), safeNextPath(r.FormValue("next")), email, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	token, err := h.Auth.CreateSession(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	auth.SetSessionCookie(w, token, h.Secure)
	// Адресат употреблён — invite-cookie (если была) больше не нужна (K9-19):
	// оставлять её висеть до истечения TTL значило бы дать ей всплыть на
	// следующем, никак не связанном входе.
	h.clearInviteNextCookie(w)
	// Возврат туда, куда человек шёл до формы входа (см. safeNextPath и
	// auth.loginWithNext): иначе глубокая ссылка — приглашение, ссылка на
	// проблему из письма алерта — теряется, и он оказывается на главной.
	redirectLocal(w, r, safeNextPath(r.FormValue("next")))
}

func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	next := safeNextPath(r.FormValue("next"))

	// SEC-L2: per-IP, per-account (ip|email) и per-email (без IP) — тот же
	// набор и тот же порядок, что в loginSubmit (см. комментарий там про
	// находку W2-B: per-IP обязан идти первым, иначе он не сдерживает рост
	// карты loginLimiter). Per-email нужен и здесь: без него распределённый
	// перебор одного приглашённого адреса с пула IP не ограничивался ничем.
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow(h.rateLimitKey(r, email)) ||
		!h.emailLimiter.Allow(limiterEmailKeyPart(email)) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.rate_limited_register"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
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
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.passwords_differ"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
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
		_ = templates.RegisterForm(i18n.T(r.Context(), "err.auth.sso_required"), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
		return
	}

	uid, err := h.Auth.Register(r.Context(), email, password)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.RegisterForm(registerErrorMessage(r.Context(), err), false, next, h.oauthButtons(r.Context())).Render(r.Context(), w)
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
	// См. комментарий в loginSubmit: адресат употреблён, invite-cookie гасим.
	h.clearInviteNextCookie(w)
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
		h.denyCrossOrigin(w, r)
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
		h.denyCrossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBodyBytes)
	if !h.parseForm(w, r) {
		return
	}
	email := r.FormValue("email")
	// SEC-L2, тот же порядок и то же обоснование, что в loginSubmit (находка
	// W2-B): per-IP лимит ПЕРВЫМ операндом ||. loginLimiter — ОДИН инстанс на
	// /login, /register и /sso (плюс profile.go) — без ipLimiter здесь тот же
	// эксплойт был доступен в обход уже закрытых путей: один IP потоком
	// выдуманных email через /sso заполнял ту же карту, что бьёт по обычному
	// входу.
	if !h.ipLimiter.Allow(h.clientIP(r)) || !h.loginLimiter.Allow("sso|"+h.rateLimitKey(r, email)) {
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
