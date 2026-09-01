package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// withShell кладёт nav.Shell (состояние app-shell: рейл + сайдбар) в
// контекст запроса для залогиненного пользователя. Анонимные запросы и
// /static/* проходят без резолвинга (пустой nav.Shell). Всё резолвится
// best-effort: любая ошибка оставляет соответствующее поле нулевым, запрос
// никогда не падает из-за недоступности навигационных данных.
func (h *Handler) withShell(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		tok, ok := auth.ReadSessionToken(r, h.Secure)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		uid, err := h.Auth.SessionUser(ctx, tok)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		email, _ := h.Auth.UserEmail(ctx, uid)

		var orgs []nav.Org
		if os, err := h.Org.OrgsOf(ctx, uid); err == nil {
			orgs = make([]nav.Org, 0, len(os))
			for _, o := range os {
				orgs = append(orgs, nav.Org{ID: o.ID, Name: o.Name})
			}
		}

		var projs []nav.Project
		if ps, err := h.Org.ProjectsForUser(ctx, uid); err == nil {
			projs = make([]nav.Project, 0, len(ps))
			for _, p := range ps {
				projs = append(projs, nav.Project{ID: p.ID, Slug: p.Slug, Name: p.Name})
			}
		}

		path := r.URL.Path
		// Источник перехода: страница эндпойнта общая для «Транзакций» и
		// «Web Vitals», трейс открывается из трёх разделов. Без пометки
		// подсветка молча уезжала в соседний подраздел (см. navOrigin).
		origin := navOrigin(r)
		area := nav.AreaForPath(path)
		if origin != "" {
			area = nav.AreaForOrigin(origin)
		}

		projID := projectIDFromPath(path)
		if projID != 0 {
			// Явный проект в пути — запоминаем выбор (см. projcookie.go).
			// Только свой: чужой id в адресе отдаст 404 в хендлере и не
			// должен затирать запомненный выбор.
			if projectInList(projs, projID) && projCookieID(r) != projID {
				setProjCookie(w, projID, h.Secure)
			}
		} else if id := projCookieID(r); id != 0 && projectInList(projs, id) {
			// Страница без проекта в пути — текущим остаётся запомненный
			// проект, а не первый из списка (fallback effectiveProjectID).
			projID = id
		}

		var orgID int64
		if oid := orgIDFromPath(path); oid != 0 {
			orgID = oid
		} else if projID != 0 {
			orgID, _ = h.Org.ProjectOrg(ctx, projID)
		}

		// Fallback for paths carrying no org/project id (e.g.
		// /projects, /profile): best-effort resolve the org from the
		// user's first project, so the org-area sidebar (Members/
		// Teams/Probes) doesn't emit /orgs/0/... links.
		if orgID == 0 && len(projs) > 0 {
			orgID, _ = h.Org.ProjectOrg(ctx, projs[0].ID)
		}

		orgMode := area == "org"

		// Best-effort: resolve whether the user can manage orgID
		// (owner/admin) to gate management links (project settings,
		// org Members/Teams/Probes) in the shell. Any error (e.g. no
		// membership) leaves canManage false.
		var canManage bool
		if orgID != 0 {
			role, err := h.Org.Role(ctx, orgID, uid)
			canManage = err == nil && (role == org.RoleOwner || role == org.RoleAdmin)
		}

		// CanOperate — проектный скоуп: участник команды текущего
		// проекта (или owner/admin). Без выбранного проекта поднимать
		// нечего — пункты мониторинга живут только внутри проекта.
		// projID уже сверен со списком доступных пользователю проектов
		// (projs, из ProjectsForUser) выше — тем же accessCondition, что и
		// CanAccessProject (задача C2: подтверждена буквальная
		// эквивалентность обоих запросов), поэтому отдельный поход в БД
		// здесь не нужен.
		canOperate := projID != 0 && projectInList(projs, projID)

		// Топбар (задача 4 nav-ia) сужает список проектов селектом
		// организации: без сужения два селекта противоречили бы друг
		// другу — организация выбрана одна, а проекты в списке ниже были
		// бы из всех организаций пользователя. projs (полный список) выше
		// уже отработал во всех проверках (cookie, orgID-фолбэк, CanOperate)
		// — здесь его заменяет только то, что попадёт в sh.Projects.
		shellProjects := projs
		if orgID != 0 {
			if ps, err := h.Org.ProjectsForUserInOrg(ctx, uid, orgID); err == nil {
				shellProjects = make([]nav.Project, 0, len(ps))
				for _, p := range ps {
					shellProjects = append(shellProjects, nav.Project{ID: p.ID, Slug: p.Slug, Name: p.Name})
				}
			}
		}

		sh := nav.Shell{
			UserEmail: email,
			Orgs:      orgs,
			Projects:  shellProjects,
			ProjectID: projID,
			OrgID:     orgID,
			Area:      area,
			OrgMode:   orgMode,
			Path:      path,
			Origin:    origin,
			// Locale feeds nav.Subsections' docs case (doc page titles
			// are localized markdown H1s, resolved via internal/docs).
			// withShell runs inside withLocale (see web.go mount line),
			// so the locale is already resolved in ctx by this point.
			Locale:         i18n.FromContext(ctx).Code,
			CanManage:      canManage,
			CanOperate:     canOperate,
			ExportsEnabled: h.Exports != nil,
			Back:           backOrigin(r, h.BaseURL, path),
		}
		next.ServeHTTP(w, r.WithContext(nav.WithShell(ctx, sh)))
	})
}

// projectInList — есть ли проект id в списке доступных пользователю. Сверка
// обязательна и при чтении cookie (общий браузер, отозванный доступ), и при
// записи (чужой id в пути не должен затирать запомненный выбор).
func projectInList(projs []nav.Project, id int64) bool {
	for _, p := range projs {
		if p.ID == id {
			return true
		}
	}
	return false
}

// projectIDFromPath парсит {id} из "/projects/{id}/..." — единственный
// прямой источник projID в этой миддлваре (см. task-2 brief: упрощённо,
// без обращения к сервисам issue/monitor/trace для detail-маршрутов).
func projectIDFromPath(path string) int64 {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "projects" {
		return 0
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// orgIDFromPath парсит {id} из "/orgs/{id}/...".
func orgIDFromPath(path string) int64 {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "orgs" {
		return 0
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// backOrigin возвращает относительный путь «откуда пришёл» из заголовка
// Referer для хлебной крошки «назад». Пусто, когда Referer нет, он с чужого
// origin, ведёт на тот же путь (перезагрузка) или на служебные адреса —
// тогда крошка падает на жёстко зашитого родителя. Возвращается только
// относительный путь+запрос (без scheme/host), проверенный на same-origin
// тем же isSameOriginURL, что и CSRF-защита POST-ов: в шаблоне он уходит в
// templ.SafeURL, поэтому чужой или протокол-относительный адрес недопустим.
func backOrigin(r *http.Request, baseURL, curPath string) string {
	ref := r.Header.Get("Referer")
	if ref == "" || !isSameOriginURL(ref, baseURL) {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	esc := u.EscapedPath() // форма для построения ссылки (сохраняет %-кодировку)
	dec := u.Path          // декодированная — для сравнения с curPath (тоже decoded)
	// «/\» отвергается наравне с «//»: браузеры трактуют его как
	// протокол-относительный адрес. Соседние функции с тем же инвариантом
	// (safeNextPath, safeRedirect, BulkRedirectTarget) режут его давно — здесь
	// он был пропущен.
	//
	// Проверяются ОБЕ формы: EscapedPath() превращает обратный слэш в «%5C», и
	// проверка только по ней пропустила бы его целиком.
	if !isLocalPath(esc) || !isLocalPath(dec) {
		return ""
	}
	// Сравниваем ДЕКОДИРОВАННЫЙ путь: curPath приходит из r.URL.Path (decoded),
	// а имена транзакций в URL эндпойнта %-кодированы — иначе крошка «назад» на
	// такой странице ссылалась бы сама на себя (escaped ≠ decoded).
	if dec == curPath || strings.HasPrefix(dec, "/static/") ||
		strings.HasPrefix(dec, "/login") || strings.HasPrefix(dec, "/logout") {
		return ""
	}
	if u.RawQuery != "" {
		return esc + "?" + u.RawQuery
	}
	return esc
}

// navOrigin — подраздел, из которого пользователь пришёл на общую страницу.
// Значение приходит из адреса (?from=), поэтому сверяется со списком
// известных: произвольная строка не должна влиять на навигацию. Сам путь
// подсветки строит nav.Subsections — только там известен проект, к которому
// привязаны ссылки сайдбара (страницы-детали живут на корневых адресах без
// идентификатора проекта).
func navOrigin(r *http.Request) string {
	switch from := r.URL.Query().Get("from"); from {
	case "web-vitals", "perf-issue", "issue", "endpoint":
		return from
	default:
		return ""
	}
}
