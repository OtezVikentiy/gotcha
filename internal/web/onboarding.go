package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// allowedPlatforms — серверный whitelist платформ онбординга; всё, что не
// входит в список (в т.ч. произвольный ввод через подменённый <select>),
// нормализуется на "other".
var allowedPlatforms = map[string]bool{
	"go":         true,
	"php":        true,
	"javascript": true,
	"python":     true,
	"other":      true,
}

func normalizePlatform(platform string) string {
	if allowedPlatforms[platform] {
		return platform
	}
	return "other"
}

// onboardingPage — GET /onboarding: у юзера без организаций форма
// «создайте организацию и первый проект», у остальных — 303 на /.
func (h *Handler) onboardingPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hasOrg, err := h.userHasProjects(r, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if hasOrg {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = templates.Onboarding("", "", "", "", "", "", h.currentEmail(r)).Render(r.Context(), w)
}

// userHasProjects — есть ли у юзера хоть один доступный проект. Используется
// как прокси для «есть организация» (см. index в web.go): своей организации
// без проекта у юзера появиться не может, потому что первый проект создаётся
// в том же onboarding-потоке.
func (h *Handler) userHasProjects(r *http.Request, uid int64) (bool, error) {
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		return false, err
	}
	return len(projects) > 0, nil
}

// onboardingSubmit — POST /onboarding: CreateOrg (юзер = owner) →
// CreateProject → CreateKey → 303 на страницу подключения SDK.
func (h *Handler) onboardingSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	orgSlug := r.FormValue("org_slug")
	orgName := r.FormValue("org_name")
	projectSlug := r.FormValue("project_slug")
	projectName := r.FormValue("project_name")
	platform := normalizePlatform(r.FormValue("platform"))

	renderInvalid := func(errMsg string) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = templates.Onboarding(errMsg, orgSlug, orgName, projectSlug, projectName, platform, h.currentEmail(r)).
			Render(r.Context(), w)
	}

	// Оба slug'а валидируются ДО обращения к БД: раньше невалидный
	// project_slug приводил к тому, что уже созданная организация
	// оставалась сиротой (CreateProject падал, CreateOrg — нет), и юзер
	// не мог ни переиспользовать её slug (ErrSlugTaken при повторной
	// попытке), ни удалить её из UI. Проверяя оба slug'а заранее, мы не
	// пишем в БД вообще ничего, если форма невалидна.
	if !org.ValidSlug(orgSlug) || !org.ValidSlug(projectSlug) {
		renderInvalid(onboardingErrorMessage(r.Context(), org.ErrInvalidSlug))
		return
	}

	o, err := h.Org.CreateOrg(r.Context(), orgSlug, orgName, uid)
	if err != nil {
		renderInvalid(onboardingErrorMessage(r.Context(), err))
		return
	}

	// С этого момента организация существует в БД. Любая ошибка ниже
	// (в норме недостижимая — оба slug'а уже провалидированы, остаётся
	// разве что гонка за slug проекта или сбой БД) компенсируется
	// удалением организации, чтобы не оставлять сироту.
	p, err := h.Org.CreateProject(r.Context(), o.ID, projectSlug, projectName, platform)
	if err != nil {
		h.compensateOrgCreate(r, o.ID)
		renderInvalid(onboardingErrorMessage(r.Context(), err))
		return
	}

	// EnsureDefaultRules (план 6) — заводит new_issue/regression правила
	// алертинга сразу для нового проекта; идемпотентна и не требует org →
	// alert зависимости (вызывается из web-слоя, а не из org.CreateProject).
	// h.Alerts проставляется отдельным полем (см. Handler) — nil-guard на
	// случай стендов, которые его не завели и не используют алерты.
	if h.Alerts != nil {
		if err := h.Alerts.EnsureDefaultRules(r.Context(), p.ID); err != nil {
			h.compensateOrgCreate(r, o.ID)
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}

	if _, err := h.Org.CreateKey(r.Context(), p.ID); err != nil {
		h.compensateOrgCreate(r, o.ID)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	http.Redirect(w, r, projectSetupPath(p.ID), http.StatusSeeOther)
}

// compensateOrgCreate — лучшее-из-возможного удаление организации, созданной
// в этом же запросе, когда последующий шаг онбординга (проект или ключ)
// провалился. Best-effort: если само удаление тоже упадёт, просто логируем —
// у ответа клиенту уже есть свой статус, ронять запрос из-за компенсации
// не нужно.
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

// projectSetupPath — путь до страницы подключения SDK конкретного проекта.
func projectSetupPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/setup"
}

// projectSetup — GET /projects/{id}/setup: DSN и сниппеты подключения SDK.
// Доступ только у тех, кто видит проект (CanAccessProject); остальным — 404,
// чтобы не палить существование чужих числовых id.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	// CanAccessProject — точечная проверка доступа; сами данные проекта
	// (имя и т.п.) берём из того же списка, что отдаёт /projects, — отдельного
	// Get-по-id в org.Service пока нет.
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	project, ok := findProject(projects, projectID)
	if !ok {
		h.notFound(w, r)
		return
	}

	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	publicKey := firstLiveKey(keys)

	var dsn string
	var snippets []templates.SetupSnippet
	if publicKey != "" {
		dsn = buildDSN(h.BaseURL, publicKey, projectID)
		snippets = setupSnippets(project.Platform, dsn)
	}

	_ = templates.ProjectSetup(project, dsn, snippets, h.currentEmail(r)).Render(r.Context(), w)
}

func findProject(projects []org.Project, id int64) (org.Project, bool) {
	for _, p := range projects {
		if p.ID == id {
			return p, true
		}
	}
	return org.Project{}, false
}

func firstLiveKey(keys []org.Key) string {
	for _, k := range keys {
		if !k.Revoked {
			return k.PublicKey
		}
	}
	return ""
}

// buildDSN собирает DSN проекта из BaseURL: {scheme}://{public_key}@{host}/{project_id}.
func buildDSN(baseURL, publicKey string, projectID int64) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + publicKey + "@" + u.Host + "/" + strconv.FormatInt(projectID, 10)
}

// setupSnippets собирает блоки «как подключить» для страницы проекта: сперва
// платформа, выбранная при создании проекта, затем остальные.
//
// Сниппеты воспроизводят то же, что написано в /docs/sdk, и это принципиально:
// собственного протокола у Gotcha нет, она принимает данные по протоколу приёма
// Sentry, поэтому подключается ОФИЦИАЛЬНЫЙ Sentry SDK нужного языка, которому
// подсовывается DSN проекта Gotcha. Раньше здесь были захардкожены три сниппета
// с пакетами, которых не существует (gotcha-go, @gotcha/browser, Gotcha\init), и
// без команд установки — новый пользователь упирался в 404 на первом же шаге.
func setupSnippets(platform, dsn string) []templates.SetupSnippet {
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
				"\t\tDsn:              \"" + dsn + "\",\n" +
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
				"    'dsn' => '" + dsn + "',\n" +
				"    'environment' => getenv('APP_ENV') ?: 'production',\n" +
				"    'traces_sample_rate' => 0.2,\n" +
				"]);\n",
		},
		"javascript": {
			Lang:    "JavaScript",
			Install: "npm install @sentry/browser",
			Code: "import * as Sentry from \"@sentry/browser\";\n\n" +
				"Sentry.init({\n" +
				"  dsn: \"" + dsn + "\",\n" +
				"  environment: \"production\",\n" +
				"  tracesSampleRate: 0.2,\n" +
				"});\n",
		},
		"python": {
			Lang:    "Python",
			Install: "pip install sentry-sdk",
			Code: "import sentry_sdk\n\n" +
				"sentry_sdk.init(\n" +
				"    dsn=\"" + dsn + "\",\n" +
				"    environment=\"production\",\n" +
				"    traces_sample_rate=0.2,\n" +
				")\n",
		},
	}

	// Порядок: платформа проекта первой — за ней пришли, её и показываем сверху.
	order := []string{"go", "php", "javascript", "python"}
	out := make([]templates.SetupSnippet, 0, len(order))
	if sn, ok := all[platform]; ok {
		out = append(out, sn)
	}
	for _, k := range order {
		if k == platform {
			continue
		}
		out = append(out, all[k])
	}
	return out
}

// projectsList — GET /projects: все проекты, доступные текущему юзеру.
// Для каждого проекта считается canManage (owner/admin организации проекта)
// — dead link fix (задача 5/2): «Org settings» рядом с проектом должна
// показываться только тем, кому эта страница вообще доступна. Роль
// запрашивается по orgID, а не по проекту, и кэшируется в rolesByOrg — юзер
// может состоять сразу в нескольких проектах одной организации, второй
// запрос той же роли не нужен.
func (h *Handler) projectsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	rolesByOrg := make(map[int64]org.Role, len(projects))
	items := make([]templates.ProjectListItem, len(projects))
	for i, p := range projects {
		role, ok := rolesByOrg[p.OrgID]
		if !ok {
			role, err = h.Org.Role(r.Context(), p.OrgID, uid)
			if err != nil && !errors.Is(err, org.ErrNotMember) {
				h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
				return
			}
			rolesByOrg[p.OrgID] = role
		}
		items[i] = templates.ProjectListItem{
			Project:   p,
			CanManage: role == org.RoleOwner || role == org.RoleAdmin,
		}
	}
	_ = templates.ProjectsList(items, h.currentEmail(r)).Render(r.Context(), w)
}
