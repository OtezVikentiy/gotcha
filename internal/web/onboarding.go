package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func (h *Handler) onboardingPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hasOrg, err := h.userHasProjects(r, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if hasOrg {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = templates.Onboarding("", "", "", "", "", "", h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) userHasProjects(r *http.Request, uid int64) (bool, error) {
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		return false, err
	}
	return len(projects) > 0, nil
}

func (h *Handler) onboardingSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// POST повторяет проверку GET: без неё юзер с проектом мог циклически заводить orgs/ключи.
	hasOrg, err := h.userHasProjects(r, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if hasOrg {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	orgSlug := r.FormValue("org_slug")
	orgName := r.FormValue("org_name")
	projectSlug := r.FormValue("project_slug")
	projectName := r.FormValue("project_name")
	// Нормализация тут только для перерисовки формы при 422 — саму нормализует CreateProject.
	platform := org.NormalizePlatform(r.FormValue("platform"))

	renderInvalid := func(errMsg string) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Onboarding(errMsg, orgSlug, orgName, projectSlug, projectName, platform, h.currentEmail(r)).
			Render(r.Context(), w)
	}

	// Оба slug'а валидируются до записи в БД: иначе невалидный project_slug оставит
	// уже созданную org сиротой без возможности переиспользовать её slug.
	if !org.ValidSlug(orgSlug) || !org.ValidSlug(projectSlug) {
		renderInvalid(onboardingErrorMessage(r.Context(), org.ErrInvalidSlug))
		return
	}

	o, err := h.Org.CreateOrg(r.Context(), orgSlug, orgName, uid)
	if err != nil {
		renderInvalid(onboardingErrorMessage(r.Context(), err))
		return
	}

	// С этого момента org в БД: любая ошибка ниже компенсируется удалением org (compensateOrgCreate).
	p, err := h.Org.CreateProject(r.Context(), o.ID, projectSlug, projectName, platform)
	if err != nil {
		h.compensateOrgCreate(r, o.ID)
		renderInvalid(onboardingErrorMessage(r.Context(), err))
		return
	}

	// h.Alerts может быть nil на стендах, которые его не завели — отсюда проверка перед вызовом.
	if h.Alerts != nil {
		if err := h.Alerts.EnsureDefaultRules(r.Context(), p.ID); err != nil {
			h.compensateOrgCreate(r, o.ID)
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

	if _, err := h.Org.CreateKeys(r.Context(), p.ID,
		org.KindBrowser, org.KindServer, org.KindAgent); err != nil {
		h.compensateOrgCreate(r, o.ID)
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, projectSetupPath(p.ID), http.StatusSeeOther)
}

func (h *Handler) projectCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	orgID, err := strconv.ParseInt(r.FormValue("org_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}

	slug := strings.TrimSpace(r.FormValue("slug"))
	name := strings.TrimSpace(r.FormValue("name"))
	platform := r.FormValue("platform")
	form := templates.FormState{
		"org_id": r.FormValue("org_id"), "slug": slug, "name": name, "platform": platform,
	}.Open("new-project")
	// Источник формы — hidden-поле origin, не Referer: тот необязателен и режется политиками.
	fromOrgPage := r.FormValue("origin") == "org_projects"
	renderFailure := func(msg string) {
		if fromOrgPage {
			h.renderOrgProjects(w, r, http.StatusUnprocessableEntity, uid, orgID, form, msg)
			return
		}
		h.renderProjectsList(w, r, http.StatusUnprocessableEntity, uid, form, msg)
	}

	if !org.ValidSlug(slug) {
		renderFailure(onboardingErrorMessage(r.Context(), org.ErrInvalidSlug))
		return
	}
	p, err := h.Org.CreateProject(r.Context(), orgID, slug, name, platform)
	if err != nil {
		renderFailure(onboardingErrorMessage(r.Context(), err))
		return
	}

	if h.Alerts != nil {
		if err := h.Alerts.EnsureDefaultRules(r.Context(), p.ID); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	if _, err := h.Org.CreateKeys(r.Context(), p.ID,
		org.KindBrowser, org.KindServer, org.KindAgent); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, projectSetupPath(p.ID), http.StatusSeeOther)
}

func (h *Handler) compensateOrgCreate(r *http.Request, orgID int64) {
	if err := h.Org.DeleteOrg(r.Context(), orgID); err != nil {
		slog.Error("onboarding: compensating org delete failed",
			"org_id", orgID, "err", err)
	}
}

func onboardingErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidSlug):
		return i18n.T(ctx, "error.slug.invalid")
	case errors.Is(err, org.ErrSlugTaken):
		return i18n.T(ctx, "error.slug.taken")
	default:
		return i18n.T(ctx, "error.onboarding.failed")
	}
}

func projectSetupPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/setup"
}

func (h *Handler) projectSetup(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		// 404, а не 403: не палим существование чужих числовых id.
		h.notFound(w, r)
		return
	}

	// Данные проекта берём из общего списка ProjectsForUser — точечного Get-по-id в org.Service нет.
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	project, ok := findProject(projects, projectID)
	if !ok {
		h.notFound(w, r)
		return
	}

	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	browserKey := liveKeyFor(keys, org.KindBrowser)
	serverKey := liveKeyFor(keys, org.KindServer)

	var browserDSN, serverDSN string
	if browserKey != "" {
		browserDSN = buildDSN(h.BaseURL, browserKey, projectID)
	}
	if serverKey != "" {
		serverDSN = buildDSN(h.BaseURL, serverKey, projectID)
	}
	var snippets []templates.SetupSnippet
	if browserDSN != "" || serverDSN != "" {
		snippets = setupSnippets(project.Platform, browserDSN, serverDSN)
	}
	// dsn шапки может быть пуст даже при непустых snippets (например, отозван свой ключ) —
	// видимость блока в шаблоне идёт по len(snippets), а не по dsn.
	dsn := serverDSN
	if project.Platform == "javascript" {
		dsn = browserDSN
	}

	// Если снипет языка проекта выпал из-за пустого DSN, а другие остались —
	// отдельно подсказываем, куда делся именно он.
	var missingPlatformKind org.KeyKind
	if len(snippets) > 0 {
		if want := sdkPlatformKind(project.Platform); want != "" {
			got := serverDSN
			if want == org.KindBrowser {
				got = browserDSN
			}
			if got == "" {
				missingPlatformKind = want
			}
		}
	}

	_ = templates.ProjectSetup(project, dsn, snippets, missingPlatformKind, h.currentEmail(r)).Render(r.Context(), w)
}

func findProject(projects []org.Project, id int64) (org.Project, bool) {
	for _, p := range projects {
		if p.ID == id {
			return p, true
		}
	}
	return org.Project{}, false
}

// Фолбэк на legacy: старые ключи без типа продолжают выдавать рабочий DSN. Ключ с
// пустым kind сюда не попадает — приём (ingest/scope.go) трактует "" как полный отказ.
func liveKeyFor(keys []org.Key, kind org.KeyKind) string {
	var legacy string
	for _, k := range keys {
		if k.Revoked {
			continue
		}
		if k.Kind == kind {
			return k.PublicKey
		}
		if k.Kind == org.KindLegacy && legacy == "" {
			legacy = k.PublicKey
		}
	}
	return legacy
}

func buildDSN(baseURL, publicKey string, projectID int64) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + publicKey + "@" + u.Host + "/" + strconv.FormatInt(projectID, 10)
}

func sdkPlatformKind(platform string) org.KeyKind {
	switch platform {
	case "javascript":
		return org.KindBrowser
	case "go", "php", "python":
		return org.KindServer
	}
	return ""
}

// browserDSN и serverDSN разведены по языкам не для косметики: JS-сниппет публикуется
// в коде страницы и не должен нести ключ с более широким допуском, чем серверному.
func setupSnippets(platform, browserDSN, serverDSN string) []templates.SetupSnippet {
	dsnFor := func(k string) string {
		switch sdkPlatformKind(k) {
		case org.KindBrowser:
			return browserDSN
		case org.KindServer:
			return serverDSN
		}
		return ""
	}
	all := map[string]templates.SetupSnippet{
		"go": {
			Lang:    "Go",
			Install: "go get github.com/getsentry/sentry-go",
			Code: "package main\n\n" +
				"import (\n" +
				"\t\"log\"\n" +
				"\t\"time\"\n\n" +
				"\t\"github.com/getsentry/sentry-go\"\n" +
				")\n\n" +
				"func main() {\n" +
				"\tif err := sentry.Init(sentry.ClientOptions{\n" +
				"\t\tDsn:              \"" + serverDSN + "\",\n" +
				"\t\tEnvironment:      \"production\",\n" +
				"\t\tTracesSampleRate: 0.2,\n" +
				"\t}); err != nil {\n" +
				"\t\tlog.Fatal(err)\n" +
				"\t}\n" +
				"\tdefer sentry.Flush(2 * time.Second)\n" +
				"}\n",
		},
		"php": {
			Lang:    "PHP",
			Install: "composer require sentry/sentry",
			Code: "<?php\n" +
				"require __DIR__ . '/vendor/autoload.php';\n\n" +
				"\\Sentry\\init([\n" +
				"    'dsn' => '" + serverDSN + "',\n" +
				"    'environment' => getenv('APP_ENV') ?: 'production',\n" +
				"    'traces_sample_rate' => 0.2,\n" +
				"]);\n",
		},
		"javascript": {
			Lang:    "JavaScript",
			Install: "npm install @sentry/browser",
			Code: "import * as Sentry from \"@sentry/browser\";\n\n" +
				"Sentry.init({\n" +
				"  dsn: \"" + browserDSN + "\",\n" +
				"  environment: \"production\",\n" +
				"  tracesSampleRate: 0.2,\n" +
				"});\n",
		},
		"python": {
			Lang:    "Python",
			Install: "pip install sentry-sdk",
			Code: "import sentry_sdk\n\n" +
				"sentry_sdk.init(\n" +
				"    dsn=\"" + serverDSN + "\",\n" +
				"    environment=\"production\",\n" +
				"    traces_sample_rate=0.2,\n" +
				")\n",
		},
	}

	order := []string{"go", "php", "javascript", "python"}
	out := make([]templates.SetupSnippet, 0, len(order))
	if sn, ok := all[platform]; ok && dsnFor(platform) != "" {
		out = append(out, sn)
	}
	for _, k := range order {
		if k == platform || dsnFor(k) == "" {
			continue
		}
		out = append(out, all[k])
	}
	return out
}

func (h *Handler) renderProjectsList(w http.ResponseWriter, r *http.Request, status int, uid int64, form templates.FormState, errMsg string) {
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	rolesByOrg := make(map[int64]org.Role, len(projects))
	items := make([]templates.ProjectListItem, len(projects))
	for i, p := range projects {
		role, ok := rolesByOrg[p.OrgID]
		if !ok {
			role, err = h.Org.Role(r.Context(), p.OrgID, uid)
			if err != nil && !errors.Is(err, org.ErrNotMember) {
				h.renderError(w, r, http.StatusInternalServerError, "")
				return
			}
			rolesByOrg[p.OrgID] = role
		}
		items[i] = templates.ProjectListItem{
			Project:   p,
			CanManage: role == org.RoleOwner || role == org.RoleAdmin,
		}
	}
	// Нужны ВСЕ организации пользователя, не только те, где уже есть проекты —
	// иначе пустая org не получит способа завести первый.
	orgs, err := h.Org.OrgsOf(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	var canCreate []templates.OrgOption
	for _, o := range orgs {
		role, ok := rolesByOrg[o.ID]
		if !ok {
			role, err = h.Org.Role(r.Context(), o.ID, uid)
			if err != nil && !errors.Is(err, org.ErrNotMember) {
				h.renderError(w, r, http.StatusInternalServerError, "")
				return
			}
			rolesByOrg[o.ID] = role
		}
		if role == org.RoleOwner || role == org.RoleAdmin {
			canCreate = append(canCreate, templates.OrgOption{ID: o.ID, Name: o.Name})
		}
	}

	w.WriteHeader(status)
	_ = templates.ProjectsList(items, canCreate, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}
