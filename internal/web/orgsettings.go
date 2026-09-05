package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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

func orgSettingsInviteRevokePath(orgID int64) string {
	return orgSettingsPath(orgID) + "/invite/revoke"
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

func validInviteEmail(email string) bool {
	return email != "" && auth.ValidEmailFormat(email)
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
	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", "", nil)
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

// requireInstanceAdminForSSO гейтит настройку/удаление per-org SSO админом
// инстанса. Само-обслуживаемая настройка SSO владельцем орга для НЕпроверенного
// на владение домена — захват аккаунта (см. ssoCallback): атакующий создал бы
// свой орг, заявил domain=victim.com со СВОИМ IdP и, пройдя domain-guard,
// залогинился бы в чужой парольный аккаунт. Поэтому федерацию настраивает только
// оператор инстанса — для доменов, которыми владеет. Возвращает false и рендерит
// ответ, если проверка не пройдена.
func (h *Handler) requireInstanceAdminForSSO(w http.ResponseWriter, r *http.Request, uid int64) bool {
	admin, err := h.Auth.UserIsInstanceAdmin(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return false
	}
	if !admin {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "err.org.sso_admin_only"))
		return false
	}
	return true
}

// orgSettingsSSO — POST /orgs/{id}/settings/sso: инстанс-админ настраивает per-org OIDC.
func (h *Handler) orgSettingsSSO(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.requireInstanceAdminForSSO(w, r, uid) {
		return
	}
	if !h.parseForm(w, r) {
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
		h.ssoProviders.invalidate(orgID) // иначе новая конфигурация применится только через ssoCacheTTL
		http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
	case errors.Is(err, org.ErrDomainTaken):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.domain_taken"), "", nil)
	case errors.Is(err, org.ErrInvalidSSO) || errors.Is(err, org.ErrInvalidRole):
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.sso_fields_required"), "", nil)
	default:
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
	}
}

// orgSettingsSSODelete — POST /orgs/{id}/settings/sso/delete: owner убирает SSO.
func (h *Handler) orgSettingsSSODelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.requireInstanceAdminForSSO(w, r, uid) {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.sso_delete.message", "confirm.delete",
			orgSettingsPath(orgID), orgSettingsPath(orgID)+"/sso/delete", nil)
		return
	}
	if err := h.Org.DeleteSSO(r.Context(), orgID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Отзыв федерации обязан действовать немедленно: без сброса кеша отозванный
	// IdP ещё до 5 минут выдавал бы логины и JIT-провижнинг участников.
	h.ssoProviders.invalidate(orgID)
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// renderOrgSettings — общий рендер страницы настроек: используется и
// GET-обработчиком, и POST-обработчиками (422 с сообщением об ошибке на
// месте, без редиректа — как логин/онбординг). POST .../invite при успехе
// тоже рендерит эту же страницу напрямую (без редиректа): одноразовый токен
// приглашения нельзя протащить через query string или Location, поэтому
// ссылка-приглашение показывается один раз, сразу в теле ответа POST.
func (h *Handler) renderOrgSettings(w http.ResponseWriter, r *http.Request, status int, orgID, uid int64, errMsg, inviteLink string, inviteForm templates.FormState) {
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
	logUsage, err := h.Org.LogUsage(r.Context(), orgID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	quotas := []templates.QuotaVM{
		{Kind: i18n.T(r.Context(), "org.quota.kind.events"), Field: "event_quota", Usage: usage, Limit: o.EventQuota},
		{Kind: i18n.T(r.Context(), "org.quota.kind.transactions"), Field: "transaction_quota", Usage: txUsage, Limit: o.TransactionQuota},
		{Kind: i18n.T(r.Context(), "org.quota.kind.metrics"), Field: "metric_quota", Usage: metricUsage, Limit: o.MetricQuota},
		{Kind: i18n.T(r.Context(), "org.quota.kind.profiles"), Field: "profile_quota", Usage: profileUsage, Limit: o.ProfileQuota},
		{Kind: i18n.T(r.Context(), "org.quota.kind.logs"), Field: "log_quota", Usage: logUsage, Limit: o.LogQuota},
	}
	banner := h.quotaBanner(r.Context(), orgID, true)
	// Выписанные приглашения. Ошибка чтения не должна ронять всю страницу
	// настроек: список приглашений полезен, но не важнее возможности управлять
	// участниками и квотами.
	invites, err := h.Org.PendingInvites(r.Context(), orgID)
	if err != nil {
		slog.Error("web: pending invites lookup failed", "org_id", orgID, "error", err)
		invites = nil
	}
	w.WriteHeader(status)
	_ = templates.OrgSettings(o, members, uid, quotas, h.EmailEnabled, errMsg, inviteLink, h.ssoSettingsVM(r, orgID, uid), h.currentEmail(r), banner, h.subjectPurgeVM(r.Context(), orgID), invites, inviteForm).Render(r.Context(), w)
}

// subjectPurgeVM собирает состояние блока удаления ПДн: предупреждение о
// критериях, которые на этом инстансе заведомо пусты. Итог самого удаления
// показывается общим сообщением о результате действия (см. flash.go).
//
// Предупреждение важнее итога: при включённых по умолчанию GOTCHA_SCRUB_IP и
// GOTCHA_SCRUB_EMAIL колонки user_email и user_ip зануляются на приёме, поэтому
// поиск субъекта по email или IP не совпадает ни с чем — а форма их спрашивает
// и раньше молча принимала.
func (h *Handler) subjectPurgeVM(ctx context.Context, orgID int64) templates.SubjectPurgeVM {
	vm := templates.SubjectPurgeVM{InertEmail: h.ScrubEmail, InertIP: h.ScrubIP}
	if h.Org == nil {
		return vm
	}
	if projects, err := h.Org.ProjectsOf(ctx, orgID); err == nil {
		vm.Projects = make([]templates.ProjectOption, 0, len(projects))
		for _, p := range projects {
			vm.Projects = append(vm.Projects, templates.ProjectOption{ID: p.ID, Name: p.Name})
		}
	} else {
		// Список проектов вспомогательный: без него форма всё ещё работает
		// (селект будет пуст), но ронять страницу настроек из-за него нельзя.
		slog.Warn("orgSettings: cannot list projects for GDPR form", "org_id", orgID, "error", err)
	}
	return vm
}

// quotaBanner собирает вьюмодель баннера про ограничение приёма для орга orgID
// (PROD-P1: конец молчаливых потерь). Возвращает nil, когда показывать нечего:
// за текущий месяц нет отклонённых элементов И (лимит событий безлимитный ИЛИ
// приём далёк от лимита). Баннер показывается, если за текущий месяц дропнут
// хотя бы один элемент любого класса (события/транзакции/метрики/профили) ЛИБО
// при заданном лимите событий использование достигло 90%. Ссылка ведёт на
// настройки орга (rate-guard). Баннер вспомогательный: любая ошибка чтения
// usage/дропов не должна ронять страницу — тогда просто возвращаем nil.
// canManage — можно ли давать ссылку на настройки организации. Для обычного
// участника ссылки нет: страница настроек требует owner/admin и отдала бы ему
// 404, а баннер при этом сам предлагает туда пойти. Текст ему всё равно
// показываем — знать, что приём ограничен, полезно всем, кто смотрит на
// пустеющий список проблем.
// droppedBreakdown — «события 1 200, профили 300»: только непустые виды.
//
// Порядок фиксированный, а не по величине: одинаковый порядок между заходами
// читается быстрее, чем перетасованный по значению.
func droppedBreakdown(ctx context.Context, d org.Dropped) string {
	parts := make([]string, 0, 4)
	for _, kind := range []struct {
		key string
		n   int64
	}{
		{org.QuotaKindEvents, d.Events},
		{org.QuotaKindTransactions, d.Transactions},
		{org.QuotaKindMetrics, d.Metrics},
		{org.QuotaKindProfiles, d.Profiles},
		{org.QuotaKindLogs, d.Logs},
	} {
		if kind.n <= 0 {
			continue
		}
		parts = append(parts, i18n.T(ctx, "org.quota.kind."+kind.key+".short")+" "+strconv.FormatInt(kind.n, 10))
	}
	if len(parts) == 0 {
		return ""
	}
	return i18n.Tf(ctx, "org.quota.dropped_breakdown", "parts", strings.Join(parts, ", "))
}

func (h *Handler) quotaBanner(ctx context.Context, orgID int64, canManage bool) *templates.QuotaBanner {
	href := orgSettingsPath(orgID)
	if !canManage {
		href = ""
	}
	now := time.Now()
	dropped, err := h.Org.DroppedUsage(ctx, orgID, now)
	if err != nil {
		slog.Warn("quotaBanner: dropped usage", "org_id", orgID, "err", err)
		return nil
	}
	total := dropped.Events + dropped.Transactions + dropped.Metrics + dropped.Profiles + dropped.Logs
	if total > 0 {
		return &templates.QuotaBanner{
			Text: i18n.Tn(ctx, "org.quota.dropped_banner", int(total)),
			// Разбивка по видам: общее число не говорит, какую квоту поднимать.
			// «Отклонено 12 400» одинаково выглядит и при исчерпанной квоте
			// профилей, и при исчерпанной квоте событий — а это разные решения.
			Detail: droppedBreakdown(ctx, dropped),
			Href:   href,
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
			Href: href,
		}
	}
	return nil
}

// ssoSettingsVM собирает данные секции SSO настроек орга (этап 10). Секция
// видна owner'у организации либо admin'у инстанса; client_secret обратно не
// отдаём (показываем «настроено»).
func (h *Handler) ssoSettingsVM(r *http.Request, orgID, uid int64) templates.SSOSettings {
	vm := templates.SSOSettings{
		RedirectURI: h.BaseURL + "/auth/oauth/" + ssoProviderPrefix + strconv.FormatInt(orgID, 10) + "/callback",
	}
	if role, err := h.Org.Role(r.Context(), orgID, uid); err == nil && role == org.RoleOwner {
		vm.IsOwner = true
	}
	// Настройку федерации выполняет только админ инстанса (см.
	// requireInstanceAdminForSSO): владельцу орга показываем статус, но форму — нет.
	if admin, err := h.Auth.UserIsInstanceAdmin(r.Context(), uid); err == nil && admin {
		vm.CanConfigure = true
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
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if targetID == uid {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.own_role"), "", nil)
		return
	}
	role := org.Role(r.FormValue("role"))
	// SetRoleAs — актёрозависимый вариант (security fix): проверяет роль
	// актёра, роль цели и last-owner защиту в ОДНОЙ транзакции с самой
	// мутацией (см. её комментарий в internal/org/member.go), закрывая TOCTOU
	// между requireOrgRole и мутацией.
	if err := h.Org.SetRoleAs(r.Context(), orgID, uid, targetID, role); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
		return
	}
	h.flashOK(w, "flash.saved", 0)
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
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	// RemoveMemberAs — тот же TOCTOU-фикс, что и у SetRoleAs выше.
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.member_remove.message", "confirm.remove",
			orgSettingsPath(orgID), orgSettingsRemovePath(orgID),
			[]templates.HiddenField{{Name: "user_id", Value: strconv.FormatInt(targetID, 10)}})
		return
	}
	if err := h.Org.RemoveMemberAs(r.Context(), orgID, uid, targetID); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
		return
	}
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsLeave — POST /orgs/{id}/settings/leave: участник (ЛЮБОЙ роли, не
// только owner/admin — потому requireOrgRole здесь НЕ применяется, в отличие
// от orgSettingsRemove) выходит из организации сам. Единственный owner получает
// 422 (ErrLastOwner) — сначала нужно передать владение. Не участник → 404 (не
// палим существование чужой организации, как requireOrgRole). Успех → 303 на /.
//
// Членства в командах этой организации снимает база: team_members_member_fk
// объявлен ON DELETE CASCADE (миграция 0029). Делать это здесь вручную не
// нужно и вредно — появится вторая копия инварианта, которая разойдётся с
// первой.
//
// Сессии удалённого участника намеренно не инвалидируются: пользователь
// бывает членом нескольких организаций, и удаление из одной не должно
// выкидывать его из остальных. Доступ проверяется на каждом запросе
// (CanAccessProject ходит в базу), поэтому живая cookie перестаёт открывать
// проекты этой организации сразу после удаления.
func (h *Handler) orgSettingsLeave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
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
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
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
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	// №27: 422 сохраняет введённое (email, роль) и показывает ошибку у самой
	// формы приглашения, а не абзацем под h1 — см. inviteForm в шаблоне.
	inviteForm := templates.FormState{"email": email, "role": r.FormValue("role")}
	if !validInviteEmail(email) {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.invalid_email"), "", inviteForm)
		return
	}
	role := org.Role(r.FormValue("role"))
	token, err := h.Org.Invite(r.Context(), orgID, email, role)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", inviteForm)
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
		// Имя организации и адрес приглашающего — в тексте письма (QA
		// MINOR-3): «Приглашение в организацию Gotcha» читалось как
		// организация с именем «Gotcha», а на мульти-org инсталляции
		// получатель вовсе не понимал, куда и кто его зовёт. Ошибки
		// подстановок не роняют отправку (письмо и так best-effort):
		// приглашение уже выписано, ссылка показана в UI.
		orgName := ""
		if o, err := h.Org.Get(r.Context(), orgID); err == nil {
			orgName = o.Name
		} else {
			slog.Warn("orgSettingsInvite: org lookup for email failed", "org_id", orgID, "err", err)
		}
		inviter, err := h.Auth.UserEmail(r.Context(), uid)
		if err != nil {
			slog.Warn("orgSettingsInvite: inviter lookup for email failed", "org_id", orgID, "err", err)
		}
		payload := map[string]any{
			// Письмо уходит на языке приглашающего: локаль адресата ещё
			// неизвестна — он в системе не зарегистрирован.
			"subject": i18n.Tf(r.Context(), "org.invite.email_subject", "org", orgName),
			"body": i18n.Tf(r.Context(), "org.invite.email_body",
				"org", orgName, "inviter", inviter, "link", inviteLink),
		}
		if err := h.Email.Send(r.Context(), notify.Target{Kind: "email", Target: email}, payload); err != nil {
			slog.Warn("orgSettingsInvite: failed to send invite email", "org_id", orgID, "err", err)
		}
	}

	h.renderOrgSettings(w, r, http.StatusOK, orgID, uid, "", inviteLink, nil)
}

// orgSettingsInviteRevoke — POST /orgs/{id}/settings/invite/revoke: отзыв
// выписанного приглашения.
//
// Раньше выписанное приглашение было невидимо и неотменяемо: ошибся в адресе —
// ссылка ушла постороннему, и сделать с этим из интерфейса было нельзя, хотя
// в сервисе способ существовал.
//
// Подтверждение двухшаговое, как у остальных необратимых действий: CSP без
// unsafe-inline не исполняет inline confirm(), поэтому первый POST рендерит
// страницу вопроса. В вопросе назван адрес — иначе он защищает от опечатки,
// которую на экране не видно.
func (h *Handler) orgSettingsInviteRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	inviteID, err := strconv.ParseInt(r.FormValue("invite_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	email := r.FormValue("email")
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "org.invite.revoke_confirm", "org.invite.revoke",
			orgSettingsPath(orgID), orgSettingsInviteRevokePath(orgID),
			[]templates.HiddenField{
				{Name: "invite_id", Value: strconv.FormatInt(inviteID, 10)},
				{Name: "email", Value: email},
			}, "email", email)
		return
	}
	if err := h.Org.RevokeInvite(r.Context(), orgID, inviteID); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid,
			i18n.T(r.Context(), "error.org.invite_not_found"), "", nil)
		return
	}
	h.flashOK(w, "flash.invite_revoked", 0)
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsQuota — POST /orgs/{id}/settings/quota: единый защитный лимит
// приёма (rate-guard). Форма несёт пять полей — event_quota /
// transaction_quota / metric_quota / profile_quota / log_quota (каждое:
// событий/транзакций/метрик/профилей/логов в месяц). Доступ только owner/admin
// (requireOrgRole — та же граница, что и у остальных настроек организации).
// Отрицательное или нечисловое значение любого поля → 422 (ErrInvalidQuota),
// причём ДО применения каких-либо изменений (сначала полностью валидируем все
// поля, потом сохраняем). Все пять применяются ОДНИМ вызовом org.SetQuotas —
// единый UPDATE, а не цикл отдельных Set*Quota/SetLogQuota, так что сбой БД
// на применении не может оставить квоты частично изменёнными (если бы форма
// сохраняла log_quota отдельным вызовом после SetQuotas, обрыв между двумя
// вызовами закоммитил бы четыре квоты и показал пользователю 422, будто не
// сохранилось ничего). Пустое/отсутствующее поле пропускается (эту квоту не
// трогаем); 0 = безлимит.
func (h *Handler) orgSettingsQuota(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	// Поля rate-guard и указатели, куда положить распарсенное значение.
	// Порядок фиксирован; nil-поле после парсинга = не прислано = не трогаем.
	var event, transaction, metric, profile, log *int64
	fields := []struct {
		name string
		dst  **int64
	}{
		{"event_quota", &event},
		{"transaction_quota", &transaction},
		{"metric_quota", &metric},
		{"profile_quota", &profile},
		{"log_quota", &log},
	}
	for _, f := range fields {
		raw := strings.TrimSpace(r.FormValue(f.name))
		if raw == "" {
			continue // поле не прислано — эту квоту не меняем
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), org.ErrInvalidQuota), "", nil)
			return
		}
		*f.dst = &v
	}
	if err := h.Org.SetQuotas(r.Context(), orgID, event, transaction, metric, profile, log); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// orgSettingsDelete — POST /orgs/{id}/settings/delete: owner-only удаление
// организации. PG-удаление (org.DeleteOrg, FK ON DELETE CASCADE снимает
// членов/проекты/ключи и т.д.) той же транзакцией ставит заявки на очистку
// телеметрии всех проектов организации — выборкой по org_id ДО удаления,
// потому что каскад уничтожает идентификаторы. Выполняет заявки фоновый
// исполнитель (telemetry.PurgeWorker).
//
// Раньше проекты перечислялись здесь отдельным запросом вне всякой транзакции,
// а телеметрия чистилась синхронно, по восемь мутаций на проект в одном
// HTTP-запросе: организация с двадцатью проектами упиралась в WriteTimeout, и
// данные непройденных проектов оставались в ClickHouse навсегда.
//
// Успех → 303 на / (роута /orgs нет — RA-7; как orgSettingsLeave).
func (h *Handler) orgSettingsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	// Двухшаговое подтверждение (см. orgSettingsLeave/renderConfirm): без
	// confirmed=yes показываем страницу подтверждения вместо удаления орга.
	// Имя организации — в тексте вопроса (K7-3, как у hostDelete).
	if r.FormValue("confirmed") != "yes" {
		o, err := h.Org.Get(r.Context(), orgID)
		if err != nil {
			if errors.Is(err, org.ErrNotFound) {
				h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.org_delete.message", "org.danger.delete_org.button",
			orgSettingsPath(orgID), orgSettingsDeletePath(orgID), nil,
			"name", o.Name)
		return
	}
	if err := h.Org.DeleteOrg(r.Context(), orgID); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.org_delete_queued", 0)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// orgSettingsPurgeSubject — POST /orgs/{id}/settings/purge-subject: owner-only
// удаление ПДн субъекта в рамках проекта. Поля формы: project_id (обязателен,
// должен принадлежать этому оргу) и хотя бы одно из email/user_id/ip. PG не
// трогается (субъектные ПДн живут в ClickHouse); вызывается best-effort
// h.Purger.PurgeSubject. Успех → 303 обратно на страницу настроек орга.
func (h *Handler) orgSettingsPurgeSubject(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.project_required"), "", nil)
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
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.subject_required"), "", nil)
		return
	}
	// Право на удаление ПДн (152-ФЗ ст.14): не выдаём успех, если удаление не
	// выполнено. Нет Purger (стенд без ClickHouse) или ошибка очистки → 5xx, а не
	// молчаливый redirect-как-успех — оператор должен знать, что ПДн НЕ удалены.
	if h.Purger == nil {
		// Как ExportSubject: подсистема очистки не сконфигурирована → 503, а не
		// молчаливый успех (ПДн субъекта НЕ удалены).
		slog.Error("orgSettingsPurgeSubject: Purger not configured, subject data NOT purged", "org_id", orgID, "project_id", projectID)
		h.renderError(w, r, http.StatusServiceUnavailable, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Двухшаговое подтверждение. Здесь оно обязательнее прочего: удаление
	// необратимо, а проект задаётся номером — опечатка 25→26 вычистила бы
	// телеметрию соседнего проекта того же орга без единого вопроса.
	if r.FormValue("confirmed") != "yes" {
		hidden := []templates.HiddenField{
			{Name: "project_id", Value: strconv.FormatInt(projectID, 10)},
		}
		if sub.Email != "" {
			hidden = append(hidden, templates.HiddenField{Name: "email", Value: sub.Email})
		}
		if sub.UserID != "" {
			hidden = append(hidden, templates.HiddenField{Name: "user_id", Value: sub.UserID})
		}
		if sub.IP != "" {
			hidden = append(hidden, templates.HiddenField{Name: "ip", Value: sub.IP})
		}
		// Показываем ИМЕННО то, что будет удалено: имя проекта, его номер и
		// заполненные критерии. Без этого вопрос нельзя было осмысленно
		// подтвердить — опечатку 25→26 на экране не по чему заметить, а
		// проверка принадлежности проекта оргу соседний проект пропускает.
		projectName := ""
		if projects, err := h.Org.ProjectsOf(r.Context(), orgID); err == nil {
			for _, p := range projects {
				if p.ID == projectID {
					projectName = p.Name
					break
				}
			}
		}
		if projectName == "" {
			projectName = i18n.T(r.Context(), "confirm.purge_subject.unknown_project")
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.purge_subject.message", "confirm.delete",
			orgSettingsPath(orgID), orgSettingsPurgeSubjectPath(orgID), hidden,
			"project", projectName,
			"project_id", strconv.FormatInt(projectID, 10),
			"criteria", subjectCriteriaText(r.Context(), sub))
		return
	}

	res, err := h.Purger.PurgeSubject(r.Context(), projectID, sub)
	if err != nil {
		slog.Error("orgSettingsPurgeSubject: failed to purge subject data", "org_id", orgID, "project_id", projectID, "err", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Пишем результат в аудит-лог операций с ПДн: у экспорта такая запись есть,
	// у удаления не было. Ноль строк — не ошибка, но это ровно тот исход, о
	// котором оператор обязан узнать: при включённом скрубинге email/IP поиск по
	// ним не совпадает ни с чем, работает только user_id.
	slog.Info("subject data purged",
		"org_id", orgID, "project_id", projectID, "criteria", subjectCriteria(sub),
		"events", res.Events, "transactions", res.Transactions, "spans", res.Spans,
		"metric_points", res.MetricPoints, "logs", res.Logs, "total", res.Total())

	// Итог показывается сообщением, а не query-параметром: параметр оставался в
	// адресе, залипал при F5 и уезжал в закладку, а ссылку вида ?purged=9999
	// можно было подсунуть владельцу и показать ему выдуманное число.
	h.flashOK(w, "flash.subject_purged", int(res.Total()))
	http.Redirect(w, r, orgSettingsPath(orgID)+"#gdpr", http.StatusSeeOther)
}

// orgSettingsExportSubject — POST /orgs/{id}/settings/export-subject: owner-only
// выгрузка всех ПДн субъекта в рамках проекта (право субъекта на доступ, 152-ФЗ
// ст. 14, RA-L11). Гейт и валидация идентичны orgSettingsPurgeSubject
// (requireOrgOwner, sameOrigin, project_id принадлежит оргу, хотя бы одно из
// email/user_id/ip). В отличие от purge — ExportSubject не best-effort: ошибку
// нельзя проглотить, отдаём 500. Успех → JSON-выгрузка как attachment.
func (h *Handler) orgSettingsExportSubject(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	projectID, err := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	if err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.project_required"), "", nil)
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
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, i18n.T(r.Context(), "err.org.subject_required"), "", nil)
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
// subjectCriteriaText — заполненные критерии субъекта человекочитаемо, для
// страницы подтверждения. ЗНАЧЕНИЯ показываются намеренно: подтверждать
// удаление ПДн, не видя, по кому оно идёт, бессмысленно. В журнал при этом
// уходят только ИМЕНА критериев (см. subjectCriteria) — там значения не нужны.
func subjectCriteriaText(ctx context.Context, sub telemetry.Subject) string {
	var parts []string
	if sub.Email != "" {
		parts = append(parts, i18n.T(ctx, "org.gdpr.field.email")+": "+sub.Email)
	}
	if sub.UserID != "" {
		parts = append(parts, i18n.T(ctx, "org.gdpr.field.user_id")+": "+sub.UserID)
	}
	if sub.IP != "" {
		parts = append(parts, i18n.T(ctx, "org.gdpr.field.ip")+": "+sub.IP)
	}
	return strings.Join(parts, ", ")
}

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
// Читает приглашение через InviteByToken (это ЧТЕНИЕ, не AcceptInvite —
// одноразовый токен нельзя тратить на простой просмотр страницы) и
// показывает, куда зовут: организацию, роль и адрес. Без этого человек
// подтверждал бы приглашение вслепую.
//
// Невалидный, просроченный и уже принятый токен дают ту же ошибку и тот же
// код ответа, что и неудачный POST (err.org.invite_invalid, 422) — иначе по
// разнице ответов GET и POST можно было бы перебором узнавать, какие токены
// вообще существуют.
func (h *Handler) inviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	// Маршрут публичный (см. web.go) — h.currentEmail здесь всегда вернула бы
	// "": auth.UserID кладёт в контекст только requireUser, а эта страница им
	// не обёрнута нарочно. currentEmailPublic резолвит сессию напрямую.
	email := h.currentEmailPublic(r)
	inv, err := h.Org.InviteByToken(r.Context(), token)
	if errors.Is(err, org.ErrInviteInvalid) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.InviteAccept(token, i18n.T(r.Context(), "err.org.invite_invalid"), email, org.InviteInfo{}).Render(r.Context(), w)
		return
	}
	if err != nil {
		// Fail closed: не знаем, действительно ли приглашение — не показываем
		// его содержимое.
		slog.Error("inviteAcceptPage: invite lookup failed", "err", err)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if email == "" {
		// K9-19: страница ниже покажет анониму ссылки «войти»/«создать
		// аккаунт» БЕЗ next=/invite/{token} в query (см. loginLinkWithNext/
		// registerLinkWithNext в auth.templ) — токен вместо этого едет в
		// HttpOnly cookie и его читает resolveAuthNext на GET /login и
		// /register (auth.go, invitecookie.go).
		h.setInviteNextCookie(w, token)
	}
	_ = templates.InviteAccept(token, "", email, inv).Render(r.Context(), w)
}

// inviteAcceptSubmit — POST /invite/{token}: org.AcceptInvite; успех → 303 /,
// невалидный/истёкший/уже использованный токен (ErrInviteInvalid) → 422
// styled-страница.
func (h *Handler) inviteAcceptSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
		msg := i18n.T(r.Context(), "err.org.invite_invalid")
		if errors.Is(err, org.ErrInviteEmailMismatch) {
			msg = i18n.T(r.Context(), "err.org.invite_other_email")
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		// inv — нулевое значение: errMsg != "" и шаблон его не использует (см.
		// комментарий у InviteAccept).
		_ = templates.InviteAccept(token, msg, email, org.InviteInfo{}).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
