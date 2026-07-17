package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func orgSettingsPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings"
}

func orgSettingsRolePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/role"
}

func orgSettingsRemovePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/remove"
}

func orgSettingsLeavePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/leave"
}

func orgSettingsInvitePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/invite"
}

func orgSettingsQuotaPath(orgID int64) string {
	return orgSettingsPath(orgID) + "/quota"
}

func orgSettingsDeletePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/delete"
}

func orgSettingsPurgeSubjectPath(orgID int64) string {
	return orgSettingsPath(orgID) + "/purge-subject"
}

func orgSettingsExportSubjectPath(orgID int64) string {
	return orgSettingsPath(orgID) + "/export-subject"
}

func inviteAcceptPath(token string) string {
	return "/invite/" + token
}

// reInviteEmail — та же намеренно простая проверка формата, что и
// auth.reEmail (не экспортирован оттуда): один @, непустые локальная часть
// и домен, в домене есть точка.
var reInviteEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func validInviteEmail(email string) bool {
	return email != "" && len(email) <= 254 && reInviteEmail.MatchString(email)
}

// orgSettingsErrorMessage переводит доменные ошибки org.Service в
// человекочитаемое сообщение для 422-страницы настроек организации.
func orgSettingsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, org.ErrLastOwner):
		return i18n.T(ctx, "error.org.last_owner")
	case errors.Is(err, org.ErrInvalidRole):
		return i18n.T(ctx, "error.org.invalid_role")
	case errors.Is(err, org.ErrNotMember):
		return i18n.T(ctx, "error.org.not_member")
	case errors.Is(err, org.ErrOwnerOnly):
		return i18n.T(ctx, "error.org.owner_only")
	case errors.Is(err, org.ErrInvalidQuota):
		return i18n.T(ctx, "error.org.invalid_quota")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// parsePathOrgID достаёт orgID из {id} пути /orgs/{id}/settings*; на
// невалидный id — 404 (тот же принцип, что и у числовых id issue/project:
// не палим существование записи форматом ответа).
func (h *Handler) parsePathOrgID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	orgID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return 0, false
	}
	return orgID, true
}

// orgSettingsPage — GET /orgs/{id}/settings: таблица участников (email,
// роль, форма смены роли, форма удаления — не для себя) и форма приглашения.
// Доступ только owner/admin (requireOrgRole).
func (h *Handler) orgSettingsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", "")
}

// requireOrgOwner — SSO-настройки доступны только владельцу орга (более узкая
// граница, чем requireOrgRole owner/admin): SSO — доверенная точка входа. Не
// owner → 404 (как прочие owner-only действия). Возвращает ok.
func (h *Handler) requireOrgOwner(w http.ResponseWriter, r *http.Request, orgID, uid int64) bool {
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil || role != org.RoleOwner {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return false
	}
	return true
}

// orgSettingsSSO — POST /orgs/{id}/settings/sso: owner настраивает per-org OIDC.
func (h *Handler) orgSettingsSSO(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := org.SSOConfig{
		OrgID:        orgID,
		Issuer:       r.FormValue("issuer"),
		ClientID:     r.FormValue("client_id"),
		ClientSecret: r.FormValue("client_secret"),
		Domain:       r.FormValue("domain"),
		DefaultRole:  r.FormValue("default_role"),
		Enforced:     r.FormValue("enforced") != "",
	}
	switch err := h.Org.UpsertSSO(r.Context(), cfg); {
	case err == nil:
		http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
	case errors.Is(err, org.ErrDomainTaken):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "домен уже используется другой организацией", "")
	case errors.Is(err, org.ErrInvalidSSO) || errors.Is(err, org.ErrInvalidRole):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "заполните issuer, client_id, client_secret и домен", "")
	default:
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// orgSettingsSSODelete — POST /orgs/{id}/settings/sso/delete: owner убирает SSO.
func (h *Handler) orgSettingsSSODelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := h.Org.DeleteSSO(r.Context(), orgID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// renderOrgSettings — общий рендер страницы настроек: используется и
// GET-обработчиком, и POST-обработчиками (422 с сообщением об ошибке на
// месте, без редиректа — как логин/онбординг). POST .../invite при успехе
// тоже рендерит эту же страницу напрямую (без редиректа): одноразовый токен
// приглашения нельзя протащить через query string или Location, поэтому
// ссылка-приглашение показывается один раз, сразу в теле ответа POST.
func (h *Handler) renderOrgSettings(w http.ResponseWriter, r *http.Request, status int, orgID, uid int64, errMsg, inviteLink string) {
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	members, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Секция «Защитный лимит приёма (rate-guard)» показывает по каждому виду
	// приёма (события/транзакции/метрики/профили) использование ЗА ТЕКУЩИЙ
	// месяц (org_usage ключуется по (org_id, period_month)) рядом с лимитом
	// (o.*Quota, уже загружены в Get выше). Ошибка чтения любого счётчика —
	// 500, чтобы не показать частично-пустую картину лимитов.
	now := time.Now()
	usage, err := h.Org.Usage(r.Context(), orgID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	txUsage, err := h.Org.TransactionUsage(r.Context(), orgID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	metricUsage, err := h.Org.MetricUsage(r.Context(), orgID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	profileUsage, err := h.Org.ProfileUsage(r.Context(), orgID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	quotas := []templates.QuotaVM{
		{Kind: "События", Field: "event_quota", Usage: usage, Limit: o.EventQuota},
		{Kind: "Транзакции", Field: "transaction_quota", Usage: txUsage, Limit: o.TransactionQuota},
		{Kind: "Метрики", Field: "metric_quota", Usage: metricUsage, Limit: o.MetricQuota},
		{Kind: "Профили", Field: "profile_quota", Usage: profileUsage, Limit: o.ProfileQuota},
	}
	banner := h.quotaBanner(r.Context(), orgID)
	w.WriteHeader(status)
	_ = templates.OrgSettings(o, members, uid, quotas, h.EmailEnabled, errMsg, inviteLink, h.ssoSettingsVM(r, orgID, uid), h.currentEmail(r), banner).Render(r.Context(), w)
}

// quotaBanner собирает вьюмодель баннера про ограничение приёма для орга orgID
// (PROD-P1: конец молчаливых потерь). Возвращает nil, когда показывать нечего:
// за текущий месяц нет отклонённых элементов И (лимит событий безлимитный ИЛИ
// приём далёк от лимита). Баннер показывается, если за текущий месяц дропнут
// хотя бы один элемент любого класса (события/транзакции/метрики/профили) ЛИБО
// при заданном лимите событий использование достигло 90%. Ссылка ведёт на
// настройки орга (rate-guard). Баннер вспомогательный: любая ошибка чтения
// usage/дропов не должна ронять страницу — тогда просто возвращаем nil.
func (h *Handler) quotaBanner(ctx context.Context, orgID int64) *templates.QuotaBanner {
	now := time.Now()
	dropped, err := h.Org.DroppedUsage(ctx, orgID, now)
	if err != nil {
		slog.Warn("quotaBanner: dropped usage", "org_id", orgID, "err", err)
		return nil
	}
	total := dropped.Events + dropped.Transactions + dropped.Metrics + dropped.Profiles
	if total > 0 {
		return &templates.QuotaBanner{
			Text: i18n.Tn(ctx, "org.quota.dropped_banner", int(total)),
			Href: orgSettingsPath(orgID),
		}
	}
	// Дропов нет — проверяем приближение к лимиту событий (0 = безлимит).
	o, err := h.Org.Get(ctx, orgID)
	if err != nil {
		slog.Warn("quotaBanner: get org", "org_id", orgID, "err", err)
		return nil
	}
	if o.EventQuota <= 0 {
		return nil
	}
	usage, err := h.Org.Usage(ctx, orgID, now)
	if err != nil {
		slog.Warn("quotaBanner: usage", "org_id", orgID, "err", err)
		return nil
	}
	// usage >= 90% лимита — целочисленно, без float: usage*10 >= quota*9.
	if usage*10 >= o.EventQuota*9 {
		return &templates.QuotaBanner{
			Text: i18n.Tf(ctx, "org.quota.near_limit",
				"used", strconv.FormatInt(usage, 10), "limit", strconv.FormatInt(o.EventQuota, 10)),
			Href: orgSettingsPath(orgID),
		}
	}
	return nil
}

// ssoSettingsVM собирает данные секции SSO настроек орга (этап 10). Секция
// видна только owner'у; client_secret обратно не отдаём (показываем «настроено»).
func (h *Handler) ssoSettingsVM(r *http.Request, orgID, uid int64) templates.SSOSettings {
	vm := templates.SSOSettings{
		RedirectURI: h.BaseURL + "/auth/oauth/" + ssoProviderPrefix + strconv.FormatInt(orgID, 10) + "/callback",
	}
	if role, err := h.Org.Role(r.Context(), orgID, uid); err == nil && role == org.RoleOwner {
		vm.IsOwner = true
	}
	if cfg, ok, err := h.Org.SSOByOrg(r.Context(), orgID); err == nil && ok {
		vm.Configured = true
		vm.Issuer = cfg.Issuer
		vm.ClientID = cfg.ClientID
		vm.Domain = cfg.Domain
		vm.DefaultRole = cfg.DefaultRole
		vm.Enforced = cfg.Enforced
	}
	return vm
}

// orgSettingsRole — POST /orgs/{id}/settings/role: user_id, role. Менять
// роль себе нельзя (422); org.SetRoleAs сам защищает последнего owner'а
// (ErrLastOwner → 422), проверяет допустимость роли (ErrInvalidRole → 422) и
// привилегию эскалации (ErrOwnerOnly → 422).
func (h *Handler) orgSettingsRole(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	if targetID == uid {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "нельзя изменить собственную роль", "")
		return
	}
	role := org.Role(r.FormValue("role"))
	// SetRoleAs — актёрозависимый вариант (security fix): проверяет роль
	// актёра, роль цели и last-owner защиту в ОДНОЙ транзакции с самой
	// мутацией (см. её комментарий в internal/org/member.go), закрывая TOCTOU
	// между requireOrgRole и мутацией.
	if err := h.Org.SetRoleAs(r.Context(), orgID, uid, targetID, role); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsRemove — POST /orgs/{id}/settings/remove: user_id. Self-remove
// больше не запрещаем отдельной проверкой (PROD-P7): org.RemoveMemberAs сам
// защищает последнего owner'а (ErrLastOwner → 422) — единственный owner,
// пытающийся удалить себя, получит 422; в остальном owner/admin может выйти
// сам. Метод также защищает привилегию эскалации (ErrOwnerOnly → 422).
// Отдельный, не требующий owner/admin выход участника — orgSettingsLeave.
func (h *Handler) orgSettingsRemove(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad user_id", http.StatusBadRequest)
		return
	}
	// RemoveMemberAs — тот же TOCTOU-фикс, что и у SetRoleAs выше.
	if err := h.Org.RemoveMemberAs(r.Context(), orgID, uid, targetID); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "")
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsLeave — POST /orgs/{id}/settings/leave: участник (ЛЮБОЙ роли, не
// только owner/admin — потому requireOrgRole здесь НЕ применяется, в отличие
// от orgSettingsRemove) выходит из организации сам. Единственный owner получает
// 422 (ErrLastOwner) — сначала нужно передать владение. Не участник → 404 (не
// палим существование чужой организации, как requireOrgRole). Успех → 303 на /.
func (h *Handler) orgSettingsLeave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline onsubmit="confirm()" — see renderConfirm): без
	// confirmed=yes показываем страницу подтверждения вместо выхода из орга.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.org_leave.message", "org.danger.leave_org.button",
			orgSettingsPath(orgID), orgSettingsLeavePath(orgID), nil)
		return
	}
	// RemoveMember — self-вариант без actor-guard (участник любой роли убирает
	// сам себя); ensureNotLastOwner внутри защищает последнего owner'а.
	if err := h.Org.RemoveMember(r.Context(), orgID, uid); err != nil {
		if errors.Is(err, org.ErrNotMember) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		// ErrLastOwner (единственный owner пытается уйти) и прочее → 422 с
		// сообщением на месте; такую страницу видит только owner (member на
		// last-owner не наткнётся), значит renderOrgSettings безопасен.
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// orgSettingsInvite — POST /orgs/{id}/settings/invite: email, role
// (admin|member). Успех рендерит ту же страницу настроек с готовой
// ссылкой-приглашением {BaseURL}/invite/{token} прямо в теле ответа, без
// редиректа (см. renderOrgSettings).
func (h *Handler) orgSettingsInvite(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if !validInviteEmail(email) {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "невалидный email", "")
		return
	}
	role := org.Role(r.FormValue("role"))
	token, err := h.Org.Invite(r.Context(), orgID, email, role)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "")
		return
	}
	inviteLink := h.BaseURL + inviteAcceptPath(token)

	// Упрощение (план 6, задача 5): полноценный outbox (internal/notify)
	// привязан к channel_id NOT NULL — он существует для алертов конкретного
	// проекта, а приглашение — организационное событие без проекта/канала.
	// Поэтому письмо шлётся СИНХРОННО напрямую через notify.EmailSender,
	// best-effort: ошибка SMTP не должна ронять сам POST — ссылка-приглашение
	// всё равно показывается в UI ниже и её можно передать вручную.
	if h.Email != nil && h.Email.Configured() {
		payload := map[string]any{
			"subject": "Приглашение в организацию Gotcha",
			"body":    "Вас пригласили в организацию Gotcha. Ссылка для принятия приглашения: " + inviteLink,
		}
		if err := h.Email.Send(r.Context(), notify.Target{Kind: "email", Target: email}, payload); err != nil {
			slog.Warn("orgSettingsInvite: failed to send invite email", "org_id", orgID, "err", err)
		}
	}

	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", inviteLink)
}

// orgSettingsQuota — POST /orgs/{id}/settings/quota: единый защитный лимит
// приёма (rate-guard). Форма несёт четыре поля — event_quota /
// transaction_quota / metric_quota / profile_quota (каждое: событий/транзакций/
// метрик/профилей в месяц). Доступ только owner/admin (requireOrgRole — та же
// граница, что и у остальных настроек организации). Отрицательное или
// нечисловое значение любого поля → 422 (ErrInvalidQuota), причём ДО применения
// каких-либо изменений (сначала полностью валидируем все поля, потом сохраняем),
// чтобы отклонённый POST не оставил часть квот изменёнными. Пустое/отсутствующее
// поле пропускается (эту квоту не трогаем); 0 = безлимит (org.Set*Quota).
func (h *Handler) orgSettingsQuota(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// Поля rate-guard и их сеттеры. Порядок фиксирован; применяем только после
	// того, как все присланные поля прошли валидацию.
	fields := []struct {
		name string
		set  func(context.Context, int64, int64) error
	}{
		{"event_quota", h.Org.SetQuota},
		{"transaction_quota", h.Org.SetTransactionQuota},
		{"metric_quota", h.Org.SetMetricQuota},
		{"profile_quota", h.Org.SetProfileQuota},
	}
	type pending struct {
		set   func(context.Context, int64, int64) error
		value int64
	}
	var toApply []pending
	for _, f := range fields {
		raw := strings.TrimSpace(r.FormValue(f.name))
		if raw == "" {
			continue // поле не прислано — эту квоту не меняем
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), org.ErrInvalidQuota), "")
			return
		}
		toApply = append(toApply, pending{set: f.set, value: v})
	}
	for _, p := range toApply {
		if err := p.set(r.Context(), orgID, p.value); err != nil {
			h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "")
			return
		}
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsDelete — POST /orgs/{id}/settings/delete: owner-only удаление
// организации. Порядок: сначала перечисляем проекты орга (после PG-удаления
// каскад снимет их из projects, и id мы уже не узнаем), затем PG-удаление
// (org.DeleteOrg, FK ON DELETE CASCADE снимает членов/проекты/ключи и т.д.),
// затем best-effort очистка телеметрии каждого проекта из ClickHouse
// (h.purgeProject). Успех → 303 на / (роута /orgs нет — RA-7; как orgSettingsLeave).
func (h *Handler) orgSettingsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	// Двухшаговое подтверждение (см. orgSettingsLeave/renderConfirm): без
	// confirmed=yes показываем страницу подтверждения вместо удаления орга.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.org_delete.message", "org.danger.delete_org.button",
			orgSettingsPath(orgID), orgSettingsDeletePath(orgID), nil)
		return
	}
	// Перечисляем проекты ДО удаления — PG-каскад удалит строки projects, и
	// id проектов станут недоступны для CH-очистки. Ошибка перечисления не
	// должна блокировать удаление орга: логируем и продолжаем (телеметрию
	// можно будет добить точечным PurgeProject позже).
	projects, err := h.Org.ProjectsOf(r.Context(), orgID)
	if err != nil {
		slog.Error("orgSettingsDelete: list projects for CH purge", "org_id", orgID, "err", err)
	}
	if err := h.Org.DeleteOrg(r.Context(), orgID); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	for _, p := range projects {
		h.purgeProject(r.Context(), p.ID)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// orgSettingsPurgeSubject — POST /orgs/{id}/settings/purge-subject: owner-only
// удаление ПДн субъекта в рамках проекта. Поля формы: project_id (обязателен,
// должен принадлежать этому оргу) и хотя бы одно из email/user_id/ip. PG не
// трогается (субъектные ПДн живут в ClickHouse); вызывается best-effort
// h.Purger.PurgeSubject. Успех → 303 обратно на страницу настроек орга.
func (h *Handler) orgSettingsPurgeSubject(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "укажите проект", "")
		return
	}
	// Проект должен принадлежать этому оргу — иначе owner орга A мог бы чистить
	// телеметрию проекта чужого орга по его id.
	if pOrg, err := h.Org.ProjectOrg(r.Context(), projectID); err != nil || pOrg != orgID {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	sub := telemetry.Subject{
		Email:  strings.TrimSpace(r.FormValue("email")),
		UserID: strings.TrimSpace(r.FormValue("user_id")),
		IP:     strings.TrimSpace(r.FormValue("ip")),
	}
	if sub.Email == "" && sub.UserID == "" && sub.IP == "" {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "укажите хотя бы одно поле субъекта (email, user_id или ip)", "")
		return
	}
	if h.Purger == nil {
		slog.Warn("orgSettingsPurgeSubject: Purger not configured, ClickHouse subject data left in place", "org_id", orgID, "project_id", projectID)
	} else if err := h.Purger.PurgeSubject(r.Context(), projectID, sub); err != nil {
		slog.Error("orgSettingsPurgeSubject: failed to purge subject data", "org_id", orgID, "project_id", projectID, "err", err)
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsExportSubject — POST /orgs/{id}/settings/export-subject: owner-only
// выгрузка всех ПДн субъекта в рамках проекта (право субъекта на доступ, 152-ФЗ
// ст. 14, RA-L11). Гейт и валидация идентичны orgSettingsPurgeSubject
// (requireOrgOwner, sameOrigin, project_id принадлежит оргу, хотя бы одно из
// email/user_id/ip). В отличие от purge — ExportSubject не best-effort: ошибку
// нельзя проглотить, отдаём 500. Успех → JSON-выгрузка как attachment.
func (h *Handler) orgSettingsExportSubject(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if !h.requireOrgOwner(w, r, orgID, uid) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "укажите проект", "")
		return
	}
	// Проект должен принадлежать этому оргу — иначе owner орга A мог бы выгрузить
	// телеметрию проекта чужого орга по его id.
	if pOrg, err := h.Org.ProjectOrg(r.Context(), projectID); err != nil || pOrg != orgID {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	sub := telemetry.Subject{
		Email:  strings.TrimSpace(r.FormValue("email")),
		UserID: strings.TrimSpace(r.FormValue("user_id")),
		IP:     strings.TrimSpace(r.FormValue("ip")),
	}
	if sub.Email == "" && sub.UserID == "" && sub.IP == "" {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, "укажите хотя бы одно поле субъекта (email, user_id или ip)", "")
		return
	}
	if h.Purger == nil {
		h.renderError(w, r, http.StatusServiceUnavailable, i18n.T(r.Context(), "error.export_unavailable"))
		return
	}
	export, err := h.Purger.ExportSubject(r.Context(), projectID, sub)
	if err != nil {
		slog.Error("orgSettingsExportSubject: failed to export subject data", "org_id", orgID, "project_id", projectID, "err", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Аудит: фиксируем ФАКТ выгрузки и её критерий, но НЕ значения ПДн — в лог
	// уходит только вид использованного идентификатора.
	slog.Info("subject data export", "org_id", orgID, "project_id", projectID, "criteria", subjectCriteria(sub))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="subject-export.json"`)
	if err := json.NewEncoder(w).Encode(export); err != nil {
		// Заголовки/статус уже отправлены — остаётся только залогировать.
		slog.Error("orgSettingsExportSubject: encode export", "org_id", orgID, "project_id", projectID, "err", err)
	}
}

// subjectCriteria — виды заполненных идентификаторов субъекта для аудит-лога
// (email/user_id/ip), БЕЗ самих значений ПДн.
func subjectCriteria(sub telemetry.Subject) []string {
	var c []string
	if sub.Email != "" {
		c = append(c, "email")
	}
	if sub.UserID != "" {
		c = append(c, "user_id")
	}
	if sub.IP != "" {
		c = append(c, "ip")
	}
	return c
}

// inviteAcceptPage — GET /invite/{token}: страница «принять приглашение».
// GET не трогает БД — токен одноразовый, тратить его на простой просмотр
// страницы нельзя; валидность проверяется только на POST.
func (h *Handler) inviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	_ = templates.InviteAccept(token, "", h.currentEmail(r)).Render(r.Context(), w)
}

// inviteAcceptSubmit — POST /invite/{token}: org.AcceptInvite; успех → 303 /,
// невалидный/истёкший/уже использованный токен (ErrInviteInvalid) → 422
// styled-страница.
func (h *Handler) inviteAcceptSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token := r.PathValue("token")
	email := h.currentEmail(r)
	if _, err := h.Org.AcceptInvite(r.Context(), token, uid, email); err != nil {
		msg := "приглашение недействительно, истекло или уже использовано"
		if errors.Is(err, org.ErrInviteEmailMismatch) {
			msg = "приглашение выписано на другой email"
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.InviteAccept(token, msg, email).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
