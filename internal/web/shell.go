package web

import (
	"net/http"
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

		var projs []nav.Project
		if ps, err := h.Org.ProjectsForUser(ctx, uid); err == nil {
			projs = make([]nav.Project, 0, len(ps))
			for _, p := range ps {
				projs = append(projs, nav.Project{ID: p.ID, Slug: p.Slug, Name: p.Name})
			}
		}

		// Путь для подсветки навигации может отличаться от фактического:
		// страница эндпойнта общая для «Транзакций» и «Web Vitals», и без
		// пометки об источнике переход из Web Vitals молча подсвечивал бы
		// «Транзакции» — пользователь видел, что его унесло в соседний
		// подраздел (см. navOriginPath).
		path := navOriginPath(r)
		area := nav.AreaForPath(path)

		projID := projectIDFromPath(r.URL.Path)

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

		sh := nav.Shell{
			UserEmail: email,
			Projects:  projs,
			ProjectID: projID,
			OrgID:     orgID,
			Area:      area,
			OrgMode:   orgMode,
			Path:      path,
			// Locale feeds nav.Subsections' docs case (doc page titles
			// are localized markdown H1s, resolved via internal/docs).
			// withShell runs inside withLocale (see web.go mount line),
			// so the locale is already resolved in ctx by this point.
			Locale:    i18n.FromContext(ctx).Code,
			CanManage: canManage,
		}
		next.ServeHTTP(w, r.WithContext(nav.WithShell(ctx, sh)))
	})
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

// navOriginPath — путь, по которому подсвечивается навигация. Обычно это путь
// запроса, но страницы, общие для нескольких подразделов, принимают пометку
// ?from=<подраздел>: она говорит, откуда пришёл пользователь, и подсветка
// остаётся на исходном пункте.
//
// Сейчас пометку использует только страница эндпойнта (из «Web Vitals»), но
// сам механизм общий: значение сверяется со списком известных подразделов,
// чтобы произвольная строка из адреса не влияла на навигацию.
func navOriginPath(r *http.Request) string {
	path := r.URL.Path
	from := r.URL.Query().Get("from")
	if from == "" {
		return path
	}
	projID := projectIDFromPath(path)
	if projID == 0 {
		return path
	}
	switch from {
	case "web-vitals":
		return "/projects/" + strconv.FormatInt(projID, 10) + "/web-vitals"
	default:
		return path
	}
}
