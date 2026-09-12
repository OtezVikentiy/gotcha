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

func (h *Handler) parsePathOrgID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	orgID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return 0, false
	}
	return orgID, true
}

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

// SSO — доверенная точка входа: граница у́же, чем requireOrgRole (owner/admin) — только owner.
func (h *Handler) requireOrgOwner(w http.ResponseWriter, r *http.Request, orgID, uid int64) bool {
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil || role != org.RoleOwner {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return false
	}
	return true
}

// Само-обслуживание SSO владельцем орга для непроверенного домена — захват аккаунта:
// атакующий заявил бы чужой domain со своим IdP и прошёл бы domain-guard.
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
	// CSP блокирует inline confirm() — первый POST рендерит страницу подтверждения.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.sso_delete.message", "confirm.delete",
			orgSettingsPath(orgID), orgSettingsPath(orgID)+"/sso/delete", nil)
		return
	}
	if err := h.Org.DeleteSSO(r.Context(), orgID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Без сброса кеша отозванный IdP ещё до ssoCacheTTL выдавал бы логины и JIT-провижининг.
	h.ssoProviders.invalidate(orgID)
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

// POST .../invite рендерит эту же страницу напрямую, без редиректа: одноразовый
// токен приглашения нельзя протащить через query string или Location.
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
	// Ошибка чтения любого счётчика usage — 500, чтобы не показать частично-пустую картину лимитов.
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
	// Ошибка чтения приглашений не должна ронять всю страницу настроек.
	invites, err := h.Org.PendingInvites(r.Context(), orgID)
	if err != nil {
		slog.Error("web: pending invites lookup failed", "org_id", orgID, "error", err)
		invites = nil
	}
	w.WriteHeader(status)
	_ = templates.OrgSettings(o, members, uid, quotas, h.EmailEnabled, errMsg, inviteLink, h.ssoSettingsVM(r, orgID, uid), h.currentEmail(r), banner, h.subjectPurgeVM(r.Context(), orgID), invites, inviteForm).Render(r.Context(), w)
}

// При включённых по умолчанию GOTCHA_SCRUB_IP/GOTCHA_SCRUB_EMAIL колонки user_email
// и user_ip зануляются на приёме — поиск субъекта по email/IP не совпадёт ни с чем.
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
		slog.Warn("orgSettings: cannot list projects for GDPR form", "org_id", orgID, "error", err)
	}
	return vm
}

// Порядок фиксированный, не по величине — так список читается одинаково между заходами.
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

// nil — показывать нечего: без дропов за месяц, и лимит событий безлимитный либо использование <90%.
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
			Text:   i18n.Tn(ctx, "org.quota.dropped_banner", int(total)),
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

// client_secret обратно не отдаём — показываем только «настроено».
func (h *Handler) ssoSettingsVM(r *http.Request, orgID, uid int64) templates.SSOSettings {
	vm := templates.SSOSettings{
		RedirectURI:       h.BaseURL + "/auth/oauth/" + ssoProviderPrefix + strconv.FormatInt(orgID, 10) + "/callback",
		SecretKeyInsecure: h.SecretKeyInsecure,
	}
	if role, err := h.Org.Role(r.Context(), orgID, uid); err == nil && role == org.RoleOwner {
		vm.IsOwner = true
	}
	// Владельцу орга показываем статус SSO, но не форму настройки — та только админу инстанса.
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
	// Роль актёра, роль цели и last-owner защита проверяются в одной транзакции с
	// мутацией — закрывает TOCTOU между requireOrgRole и записью.
	if err := h.Org.SetRoleAs(r.Context(), orgID, uid, targetID, role); err != nil {
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, orgSettingsPath(orgID), http.StatusSeeOther)
}

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
	// Тот же TOCTOU-фикс, что у SetRoleAs; CSP блокирует inline confirm().
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

// Членства в командах снимает каскад БД (team_members_member_fk ON DELETE CASCADE) —
// дублировать это здесь не нужно, инвариант разойдётся с БД.
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
	// CSP блокирует inline confirm().
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.org_leave.message", "org.danger.leave_org.button",
			orgSettingsPath(orgID), orgSettingsLeavePath(orgID), nil)
		return
	}
	// Сессии участника намеренно не инвалидируются: доступ проверяется на каждом запросе.
	if err := h.Org.RemoveMember(r.Context(), orgID, uid); err != nil {
		if errors.Is(err, org.ErrNotMember) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderOrgSettings(w, r, http.StatusUnprocessableEntity, orgID, uid, orgSettingsErrorMessage(r.Context(), err), "", nil)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

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

	// Письмо шлётся синхронно best-effort: сбой SMTP не роняет POST — ссылка-приглашение
	// всё равно показана в UI ниже.
	if h.Email != nil && h.Email.Configured() {
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
		// Письмо уходит на языке приглашающего: локаль адресата ещё неизвестна — он не зарегистрирован.
		payload := map[string]any{
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

// Валидация всех пяти полей идёт до применения; SetQuotas — один UPDATE, а не цикл
// Set*Quota, чтобы сбой БД не оставил квоты частично изменёнными.
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
	// nil-поле после парсинга = не прислано = эту квоту не трогаем.
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

// Заявки на очистку телеметрии ставятся ДО удаления org (по org_id) — каскад
// уничтожает project_id; выполняет их фоновый telemetry.PurgeWorker.
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
	// Имя организации называется в тексте вопроса подтверждения.
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
	// Право на удаление ПДн (152-ФЗ ст.14): не выдаём успех, если удаление не выполнено —
	// нет Purger или ошибка очистки → 5xx, не молчаливый redirect-как-успех.
	if h.Purger == nil {
		slog.Error("orgSettingsPurgeSubject: Purger not configured, subject data NOT purged", "org_id", orgID, "project_id", projectID)
		h.renderError(w, r, http.StatusServiceUnavailable, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Проект задаётся номером — опечатка вычистила бы телеметрию соседнего проекта того же орга.
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
	// Ноль строк — не ошибка, но при включённом скрубинге email/IP поиск по ним не
	// совпадает ни с чем — работает только user_id, оператор должен это увидеть.
	slog.Info("subject data purged",
		"org_id", orgID, "project_id", projectID, "criteria", subjectCriteria(sub),
		"events", res.Events, "transactions", res.Transactions, "spans", res.Spans,
		"metric_points", res.MetricPoints, "logs", res.Logs, "total", res.Total())

	// Не query-параметром: ссылку вида ?purged=9999 можно подсунуть владельцу как выдуманный итог.
	h.flashOK(w, "flash.subject_purged", int(res.Total()))
	http.Redirect(w, r, orgSettingsPath(orgID)+"#gdpr", http.StatusSeeOther)
}

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

// Значения показываются намеренно (для confirm), но не в аудит-логе — там только имена критериев.
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

// Невалидный, просроченный и уже принятый токен дают ту же ошибку и код ответа, что и
// неудачный POST — иначе по разнице ответов GET/POST можно перебором узнать, какие токены существуют.
func (h *Handler) inviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	// Маршрут публичный, requireUser его не оборачивает — h.currentEmail тут всегда вернула бы "".
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
		// Токен едет в HttpOnly cookie, а не в next= query — его читает resolveAuthNext
		// на GET /login и /register.
		h.setInviteNextCookie(w, token)
	}
	_ = templates.InviteAccept(token, "", email, inv).Render(r.Context(), w)
}

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
		// Нулевой InviteInfo{}: шаблон его не использует, когда errMsg != "".
		_ = templates.InviteAccept(token, msg, email, org.InviteInfo{}).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
