package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
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
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
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
		renderInvalid(onboardingErrorMessage(org.ErrInvalidSlug))
		return
	}

	o, err := h.Org.CreateOrg(r.Context(), orgSlug, orgName, uid)
	if err != nil {
		renderInvalid(onboardingErrorMessage(err))
		return
	}

	// С этого момента организация существует в БД. Любая ошибка ниже
	// (в норме недостижимая — оба slug'а уже провалидированы, остаётся
	// разве что гонка за slug проекта или сбой БД) компенсируется
	// удалением организации, чтобы не оставлять сироту.
	p, err := h.Org.CreateProject(r.Context(), o.ID, projectSlug, projectName, platform)
	if err != nil {
		h.compensateOrgCreate(r, o.ID)
		renderInvalid(onboardingErrorMessage(err))
		return
	}

	if _, err := h.Org.CreateKey(r.Context(), p.ID); err != nil {
		h.compensateOrgCreate(r, o.ID)
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
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

func onboardingErrorMessage(err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidSlug):
		return "slug должен состоять из строчных латинских букв, цифр и дефисов (1..64 символа)"
	case errors.Is(err, org.ErrSlugTaken):
		return "такой slug уже занят"
	default:
		return "не удалось создать организацию или проект"
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
		http.NotFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}

	// CanAccessProject — точечная проверка доступа; сами данные проекта
	// (имя и т.п.) берём из того же списка, что отдаёт /projects, — отдельного
	// Get-по-id в org.Service пока нет.
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	project, ok := findProject(projects, projectID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	publicKey := firstLiveKey(keys)

	var dsn, goSnip, phpSnip, jsSnip string
	if publicKey != "" {
		dsn = buildDSN(h.BaseURL, publicKey, projectID)
		goSnip = goSnippet(dsn)
		phpSnip = phpSnippet(dsn)
		jsSnip = jsSnippet(dsn)
	}

	_ = templates.ProjectSetup(project, dsn, goSnip, phpSnip, jsSnip, h.currentEmail(r)).Render(r.Context(), w)
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

func goSnippet(dsn string) string {
	return "package main\n\n" +
		"import gotcha \"gitflic.ru/otezvikentiy/gotcha-go\"\n\n" +
		"func main() {\n" +
		"\tgotcha.Init(gotcha.Options{DSN: \"" + dsn + "\"})\n" +
		"\tdefer gotcha.Flush()\n" +
		"}\n"
}

func phpSnippet(dsn string) string {
	return "<?php\n" +
		"require 'vendor/autoload.php';\n\n" +
		"Gotcha\\init(['dsn' => '" + dsn + "']);\n"
}

func jsSnippet(dsn string) string {
	return "import * as Gotcha from \"@gotcha/browser\";\n\n" +
		"Gotcha.init({ dsn: \"" + dsn + "\" });\n"
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
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	rolesByOrg := make(map[int64]org.Role, len(projects))
	items := make([]templates.ProjectListItem, len(projects))
	for i, p := range projects {
		role, ok := rolesByOrg[p.OrgID]
		if !ok {
			role, err = h.Org.Role(r.Context(), p.OrgID, uid)
			if err != nil && !errors.Is(err, org.ErrNotMember) {
				h.renderError(w, r, http.StatusInternalServerError, "internal error")
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
