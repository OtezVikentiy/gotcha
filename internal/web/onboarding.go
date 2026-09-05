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
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// То же условие, что и у GET: онбординг — поток для того, у кого ещё нет
	// ни одного проекта. Раньше проверял только GET, поэтому приглашённый
	// участник мог в цикле заводить себе организации, проекты и ключи приёма —
	// форма ему не показывалась, но принимался POST.
	hasOrg, err := h.userHasProjects(r, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
	// Нормализация принадлежит домену (org.CreateProject, №73); здесь она
	// нужна только чтобы перерисовка формы при 422 не получила сырое значение.
	platform := org.NormalizePlatform(r.FormValue("platform"))

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

	if _, err := h.Org.CreateKeys(r.Context(), p.ID,
		org.KindBrowser, org.KindServer, org.KindAgent); err != nil {
		h.compensateOrgCreate(r, o.ID)
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	http.Redirect(w, r, projectSetupPath(p.ID), http.StatusSeeOther)
}

// projectCreate — POST /projects/new: заводит проект в СУЩЕСТВУЮЩЕЙ
// организации.
//
// До этого создать проект можно было только через онбординг, а он открывается
// лишь тому, у кого нет ни одного проекта. То есть добавить второй сервис в
// уже работающую установку было нельзя вообще: ни кнопки, ни маршрута. Для
// продукта, который наблюдает за сервисами, «добавить ещё один сервис» —
// повторяющийся сценарий, а не разовая настройка.
//
// Организация приходит из формы (у человека их может быть несколько), поэтому
// права проверяются по ней, а не по сессии: requireOrgRole пускает только
// owner/admin — тех же, кто и так управляет проектами организации.
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
	// Откуда пришла форма — по её hidden-полю origin (K7-2): с карточной
	// страницы организации ошибка возвращает ТУ ЖЕ страницу этой организации
	// (renderOrgProjects), а не плоский список проектов всех организаций.
	// Поле, а не Referer: заголовок необязателен и режется политиками.
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

	// Дальше — то же, что делает онбординг для своего первого проекта: правила
	// алертинга по умолчанию и три ключа приёма, по одному на класс источника.
	// Без ключей страница подключения SDK показала бы проект без DSN, то есть
	// бесполезный.
	if h.Alerts != nil {
		if err := h.Alerts.EnsureDefaultRules(r.Context(), p.ID); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	if _, err := h.Org.CreateKeys(r.Context(), p.ID,
		org.KindBrowser, org.KindServer, org.KindAgent); err != nil {
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
	// Шапке страницы показываем DSN, соответствующий платформе проекта:
	// «главного» DSN у проекта больше нет, каждый сниппет несёт свой. dsn
	// может быть пуст и при непустых snippets (например, у JS-проекта
	// отозван браузерный ключ, а серверный жив) — гейт видимости всего блока
	// в шаблоне идёт по len(snippets), а не по dsn, шапка в этом случае
	// просто не рисуется.
	dsn := serverDSN
	if project.Platform == "javascript" {
		dsn = browserDSN
	}

	// missingPlatformKind — снипет для языка САМОГО проекта не попал в
	// snippets (setupSnippets отбросил его из-за пустого DSN), а другие
	// снипеты при этом есть: страница обязана объяснить, куда делся именно
	// этот язык, а не молча показать чужие. Если snippets пуст вовсе,
	// работает общее пустое состояние (len(snippets)==0 в шаблоне) — эта
	// более узкая подсказка ему не нужна.
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

// liveKeyFor — public_key живого ключа, которым следует пользоваться в
// сценарии, требующем тип kind: первый живой ключ нужного типа → иначе первый
// живой legacy → иначе "" (вызывающий показывает пустое состояние с кнопкой
// «выпустить ключ»).
//
// Фолбэк на legacy даёт переход без простоя: проект, чьи ключи выпущены до
// появления типов, продолжает видеть рабочий DSN на всех страницах. Ключ с
// незаданным типом сюда НЕ попадает: приём (internal/ingest/scope.go)
// трактует "" как отказ по всему — незаданный тип в матрице скоупа не
// заведён вовсе, — и предложить пользователю DSN, который приём молча
// отобьёт 403, хуже, чем показать пустое состояние. Сегодня недостижимо
// (project_keys.kind — NOT NULL с CHECK, дефолт вставляет литерал 'legacy'),
// правка — на случай, если ветка когда-нибудь станет достижимой (тестовый
// или будущий конструктор, забывший проставить тип).
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

// buildDSN собирает DSN проекта из BaseURL: {scheme}://{public_key}@{host}/{project_id}.
func buildDSN(baseURL, publicKey string, projectID int64) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + publicKey + "@" + u.Host + "/" + strconv.FormatInt(projectID, 10)
}

// sdkPlatformKind — тип ключа, которым подключается язык SDK platform:
// browser для javascript (сниппет исполняется в браузере), server — для
// остальных известных языков (go/php/python). Пустая строка — platform не
// входит в набор, для которого вообще есть сниппет (например, "other"): для
// такой платформы нет ни своего сниппета, ни смысла требовать под неё ключ.
func sdkPlatformKind(platform string) org.KeyKind {
	switch platform {
	case "javascript":
		return org.KindBrowser
	case "go", "php", "python":
		return org.KindServer
	}
	return ""
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
//
// browserDSN и serverDSN разведены по языкам не для косметики: JS-сниппет
// исполняется в браузере, то есть публикуется в коде страницы, — ему нужен
// DSN с ключом browser, у которого нет прав, доступных серверному ключу.
// Отдать серверный ключ в JS-сниппет значило бы опубликовать в вебе ключ с
// более широким допуском, чем ему требуется.
//
// Сниппет, чей DSN пуст (нет живого ключа нужного типа — browser для JS,
// server для остальных), в результат НЕ попадает: сниппет с dsn: "" выглядит
// готовым к копированию и молча не работает — это ловушка, а не информация
// (ревью задачи 5, круг 3). Если из-за этого не осталось ни одного сниппета,
// вызывающий (projectSetup) показывает пустое состояние — гейт по
// len(snippets), уже существующий в шаблоне.
func setupSnippets(platform, browserDSN, serverDSN string) []templates.SetupSnippet {
	// dsnFor — какой DSN нужен языку k (см. sdkPlatformKind): пуст, если для
	// k нет живого ключа нужного типа — тогда язык k в результат не попадёт.
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

	// Порядок: платформа проекта первой — за ней пришли, её и показываем
	// сверху. Язык, для которого dsnFor пуст (нет живого ключа нужного
	// типа), в результат не попадает вовсе.
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

// renderProjectsList — общий рендер плоского списка проектов пользователя.
// GET /projects больше сюда не ведёт (см. projectsRedirect, orgprojects.go —
// задача 5 nav-ia): дверью служит список проектов организации. Эта функция
// осталась единственной точкой рендера для 422 POST /projects/new — форма
// создания возвращается открытой на том же плоском списке, откуда бы её ни
// открыли (та же логика, что и у renderAlerts).
func (h *Handler) renderProjectsList(w http.ResponseWriter, r *http.Request, status int, uid int64, form templates.FormState, errMsg string) {
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
	// Организации, в которых человек может завести проект: те же owner/admin,
	// что и в rolesByOrg — но перечислить надо ВСЕ его организации, а не
	// только те, где у него уже есть проекты, иначе пустая организация
	// осталась бы без способа завести в ней первый проект.
	orgs, err := h.Org.OrgsOf(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	var canCreate []templates.OrgOption
	for _, o := range orgs {
		role, ok := rolesByOrg[o.ID]
		if !ok {
			role, err = h.Org.Role(r.Context(), o.ID, uid)
			if err != nil && !errors.Is(err, org.ErrNotMember) {
				h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
