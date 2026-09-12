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

// Анонимные запросы и /static/* проходят без резолвинга. Всё best-effort: ошибка оставляет
// поле нулевым, запрос никогда не падает из-за навигационных данных.
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
				projs = append(projs, nav.Project{ID: p.ID, Slug: p.Slug, Name: p.Name, OrgID: p.OrgID})
			}
		}

		path := r.URL.Path
		// Страница эндпойнта общая для «Транзакций» и «Web Vitals» — без пометки подсветка
		// молча уезжала в соседний подраздел (см. navOrigin).
		origin := navOrigin(r)
		area := nav.AreaForPath(path)
		if origin != "" {
			area = nav.AreaForOrigin(origin)
		}

		projID := projectIDFromPath(path)
		if projID != 0 {
			// Только свой проект — чужой id в адресе отдаст 404 в хендлере и не должен затирать
			// запомненный выбор.
			if projectInList(projs, projID) && projCookieID(r) != projID {
				setProjCookie(w, projID, h.Secure)
			}
		} else if id := projCookieID(r); id != 0 && projectInList(projs, id) {
			projID = id
		}

		var orgID int64
		if oid := orgIDFromPath(path); oid != 0 {
			orgID = oid
		} else if projID != 0 {
			orgID, _ = h.Org.ProjectOrg(ctx, projID)
		}

		// Без org/project id в пути резолвим org по первому проекту пользователя, чтобы
		// сайдбар не сгенерировал /orgs/0/... ссылки.
		if orgID == 0 && len(projs) > 0 {
			orgID, _ = h.Org.ProjectOrg(ctx, projs[0].ID)
		}

		// Топбар сужает список проектов селектом организации — иначе два селекта противоречили
		// бы друг другу. projs уже несёт OrgID, второй запрос не нужен.
		shellProjects := projs
		if orgID != 0 {
			shellProjects = make([]nav.Project, 0, len(projs))
			for _, p := range projs {
				if p.OrgID == orgID {
					shellProjects = append(shellProjects, p)
				}
			}
		}

		// projID из куки может относиться к другой организации, чем уже разрешённый orgID —
		// тогда шапка и рейл расходятся молча; вне shellProjects проект сбрасывается.
		if orgID != 0 && projID != 0 && !projectInList(shellProjects, projID) {
			projID = 0
		}

		// canManage гейтит management-ссылки шапки (project settings, org Members/Teams/Probes).
		var canManage bool
		if orgID != 0 {
			role, err := h.Org.Role(ctx, orgID, uid)
			canManage = err == nil && (role == org.RoleOwner || role == org.RoleAdmin)
		}

		// projID уже сверен со списком доступных проектов (projs) — тем же условием, что и
		// CanAccessProject, поэтому отдельный поход в БД здесь не нужен.
		canOperate := projID != 0 && projectInList(projs, projID)

		sh := nav.Shell{
			UserEmail: email,
			Orgs:      orgs,
			Projects:  shellProjects,
			ProjectID: projID,
			OrgID:     orgID,
			Area:      area,
			Path:      path,
			Origin:    origin,
			// withShell работает внутри withLocale (web.go), так что locale уже резолвлен.
			Locale:                    i18n.FromContext(ctx).Code,
			CanManage:                 canManage,
			CanOperate:                canOperate,
			ExportsEnabled:            h.Exports != nil,
			ProfileRegressionsEnabled: h.ProfileRegressions != nil,
			Back:                      backOrigin(r, h.BaseURL, path),
		}
		next.ServeHTTP(w, r.WithContext(nav.WithShell(ctx, sh)))
	})
}

// Сверка обязательна и при чтении cookie (общий браузер, отозванный доступ), и при записи.
func projectInList(projs []nav.Project, id int64) bool {
	for _, p := range projs {
		if p.ID == id {
			return true
		}
	}
	return false
}

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

// Пусто при отсутствии/чужом Referer, том же пути или служебных адресах. Путь проверен
// isSameOriginURL (как CSRF) — уходит в templ.SafeURL, чужой адрес недопустим.
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
	// «/\» — тоже протокол-относительный адрес для браузера; проверяем ОБЕ формы: EscapedPath()
	// превращает его в %5C, проверка только по ней пропустила бы его целиком.
	if !isLocalPath(esc) || !isLocalPath(dec) {
		return ""
	}
	// Сравниваем декодированный путь: curPath из r.URL.Path тоже decoded, а имена транзакций в
	// URL %-кодированы — иначе крошка ссылалась бы сама на себя.
	if dec == curPath || strings.HasPrefix(dec, "/static/") ||
		strings.HasPrefix(dec, "/login") || strings.HasPrefix(dec, "/logout") {
		return ""
	}
	if u.RawQuery != "" {
		return esc + "?" + u.RawQuery
	}
	return esc
}

// Значение из ?from= сверяется со списком известных — произвольная строка не должна влиять
// на навигацию; сам путь подсветки строит nav.Subsections.
func navOrigin(r *http.Request) string {
	switch from := r.URL.Query().Get("from"); from {
	case "web-vitals", "perf-issue", "issue", "endpoint":
		return from
	default:
		return ""
	}
}
