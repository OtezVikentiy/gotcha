// Package web — серверный (SSR) UI поверх auth/org/issue/event: роутер,
// страницы аутентификации, статика. Каждая страница работает без JS
// (обычные формы и ссылки); htmx используется только как прогрессивное
// улучшение.
package web

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

//go:embed static
var staticFiles embed.FS

// Handler — весь SSR UI.
type Handler struct {
	Auth    *auth.Service
	Org     *org.Service
	Issues  *issue.Service
	Events  *event.Query
	BaseURL string
	Secure  bool // Secure = strings.HasPrefix(BaseURL, "https://")

	loginLimiter *rateLimiter
}

// New собирает Handler. BaseURL используется для sameOrigin-проверки POST-ов
// и для выставления Secure-флага сессионной cookie.
func New(authSvc *auth.Service, orgSvc *org.Service, issueSvc *issue.Service, events *event.Query, baseURL string) *Handler {
	return &Handler{
		Auth:         authSvc,
		Org:          orgSvc,
		Issues:       issueSvc,
		Events:       events,
		BaseURL:      baseURL,
		Secure:       strings.HasPrefix(baseURL, "https://"),
		loginLimiter: newRateLimiter(time.Now, 5, time.Minute),
	}
}

// Register навешивает маршруты задачи 4 на mux. Все они собираются на
// внутреннем ServeMux и монтируются на переданный mux одним catch-all "/" —
// это даёт единую точку для securityHeaders (весь ответ Handler'а несёт
// базовые security-заголовки) и для стилизованной 404-страницы на
// незарегистрированных путях (иначе достался бы голый "404 page not found"
// от stdlib ServeMux).
func (h *Handler) Register(mux *http.ServeMux) {
	inner := http.NewServeMux()

	inner.HandleFunc("GET /login", h.loginPage)
	inner.HandleFunc("POST /login", h.loginSubmit)
	inner.HandleFunc("GET /register", h.registerPage)
	inner.HandleFunc("POST /register", h.registerSubmit)
	inner.HandleFunc("POST /logout", h.logout)

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embedded static assets missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticSub))
	inner.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(fileServer)))

	inner.Handle("GET /{$}", h.requireUser(http.HandlerFunc(h.index)))

	inner.Handle("GET /profile", h.requireUser(http.HandlerFunc(h.profilePage)))
	inner.Handle("POST /profile/password", h.requireUser(http.HandlerFunc(h.profilePasswordSubmit)))
	inner.Handle("POST /profile/sessions/revoke", h.requireUser(http.HandlerFunc(h.profileSessionsRevoke)))

	inner.Handle("GET /onboarding", h.requireUser(http.HandlerFunc(h.onboardingPage)))
	inner.Handle("POST /onboarding", h.requireUser(http.HandlerFunc(h.onboardingSubmit)))
	inner.Handle("GET /projects", h.requireUser(http.HandlerFunc(h.projectsList)))
	inner.Handle("GET /projects/{id}/setup", h.requireUser(http.HandlerFunc(h.projectSetup)))
	inner.Handle("GET /projects/{id}/issues", h.requireUser(http.HandlerFunc(h.issuesList)))
	inner.Handle("POST /projects/{id}/issues/bulk", h.requireUser(http.HandlerFunc(h.issuesBulk)))
	inner.Handle("GET /issues/{id}", h.requireUser(http.HandlerFunc(h.issueDetail)))
	inner.Handle("POST /issues/{id}/status", h.requireUser(http.HandlerFunc(h.issueSetStatus)))
	inner.Handle("POST /issues/{id}/assign", h.requireUser(http.HandlerFunc(h.issueAssign)))

	inner.Handle("GET /orgs/{id}/settings", h.requireUser(http.HandlerFunc(h.orgSettingsPage)))
	inner.Handle("POST /orgs/{id}/settings/role", h.requireUser(http.HandlerFunc(h.orgSettingsRole)))
	inner.Handle("POST /orgs/{id}/settings/remove", h.requireUser(http.HandlerFunc(h.orgSettingsRemove)))
	inner.Handle("POST /orgs/{id}/settings/invite", h.requireUser(http.HandlerFunc(h.orgSettingsInvite)))
	inner.Handle("GET /invite/{token}", h.requireUser(http.HandlerFunc(h.inviteAcceptPage)))
	inner.Handle("POST /invite/{token}", h.requireUser(http.HandlerFunc(h.inviteAcceptSubmit)))

	inner.Handle("GET /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsPage)))
	inner.Handle("POST /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsCreate)))
	inner.Handle("POST /teams/{id}/members", h.requireUser(http.HandlerFunc(h.teamMembersAdd)))
	inner.Handle("POST /teams/{id}/members/remove", h.requireUser(http.HandlerFunc(h.teamMembersRemove)))
	inner.Handle("POST /teams/{id}/projects", h.requireUser(http.HandlerFunc(h.teamProjectsAttach)))
	inner.Handle("POST /teams/{id}/projects/detach", h.requireUser(http.HandlerFunc(h.teamProjectsDetach)))

	inner.Handle("GET /projects/{id}/settings", h.requireUser(http.HandlerFunc(h.projectSettingsPage)))
	inner.Handle("POST /projects/{id}/settings/rename", h.requireUser(http.HandlerFunc(h.projectSettingsRename)))
	inner.Handle("POST /projects/{id}/settings/keys", h.requireUser(http.HandlerFunc(h.projectSettingsKeyCreate)))
	inner.Handle("POST /projects/{id}/settings/keys/revoke", h.requireUser(http.HandlerFunc(h.projectSettingsKeyRevoke)))

	// Fallback: любой путь, не покрытый паттернами выше, — стилизованная 404.
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
	})

	mux.Handle("/", h.securityHeaders(inner))
}

// cacheControl проставляет Cache-Control на статику.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// cspHeader — все страницы Gotcha загружают только собственные ресурсы:
// app.css и htmx.min.js отдаются с того же origin (см. layout.templ), и ни
// один шаблон не использует inline <script>/<style> или style="" — поэтому
// 'self' без 'unsafe-inline' ничего не ломает. base-uri 'none' и
// frame-ancestors 'none' закрывают base-tag injection и clickjacking (второе
// дублирует X-Frame-Options для браузеров без поддержки CSP).
const cspHeader = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders проставляет базовые защитные заголовки на весь ответ:
// запрет MIME-sniffing, запрет встраивания в <iframe> (защита от
// clickjacking), урезанный Referrer при переходах на чужие origin'ы и CSP.
// Strict-Transport-Security добавляется только когда h.Secure (BaseURL
// начинается с https://) — на голом HTTP-деплое (например, за прокси без
// TLS) отправлять HSTS нельзя, браузер надолго заблокирует http:// доступ.
func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Content-Security-Policy", cspHeader)
		if h.Secure {
			hdr.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// renderError отдаёт стилизованную страницу ошибки (layout + сообщение) с
// заданным HTTP-статусом — замена голому http.Error, которое ломает вид
// сайта на ошибках.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	_ = templates.ErrorPage(status, msg, h.currentEmail(r)).Render(r.Context(), w)
}

// index — GET /{$}: без доступных проектов ведёт себя по-разному в
// зависимости от того, есть ли у юзера организация. Юзер вовсе без
// организаций уводится на /onboarding — ему ещё только предстоит завести
// первую организацию и проект. Юзер, который уже состоит в чужой или
// собственной организации, но не привязан ни к одному проекту (например,
// admin ещё не добавил его ни в одну команду), видит стилизованную страницу
// «нет доступных проектов» вместо повторного онбординга — заводить вторую
// организацию ему не нужно. При наличии доступных проектов — редирект на
// issues первого из них; окончательный роутинг появится в задаче 5.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
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
	if len(projects) == 0 {
		orgs, err := h.Org.OrgsOf(r.Context(), uid)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		if len(orgs) > 0 {
			_ = templates.NoProjects(h.currentEmail(r)).Render(r.Context(), w)
			return
		}
		http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, projectIssuesPath(projects[0].ID), http.StatusSeeOther)
}

// projectIssuesPath — предварительный путь до issue-листинга проекта;
// окончательный роутинг появится в задаче 5.
func projectIssuesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/issues"
}

// currentEmail — email текущего юзера для шапки (форма logout). Пустая
// строка на любую ошибку (нет сессии в контексте, юзер удалён, сбой БД) —
// эта функция обслуживает только рендер шапки и не должна ронять страницу.
func (h *Handler) currentEmail(r *http.Request) string {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		return ""
	}
	email, err := h.Auth.UserEmail(r.Context(), uid)
	if err != nil {
		return ""
	}
	return email
}

// requireUser оборачивает auth.Service.RequireUser: для htmx-запросов
// (HX-Request: true) вместо 303-редиректа на /login отдаёт 200 с заголовком
// HX-Redirect — htmx сам выполнит переход, а не покажет частичный HTML.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	inner := h.Auth.RequireUser(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hx := r.Header.Get("HX-Request") == "true"
		if !hx {
			inner.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(&hxRedirectWriter{ResponseWriter: w}, r)
	})
}

// requireOrgRole проверяет роль userID в организации orgID: доступ к
// настройкам организации (участники, роли, приглашения) есть только у
// owner/admin. Любая другая роль или отсутствие членства (org.ErrNotMember) —
// 404, тот же принцип, что и у CanAccessProject: не палим существование
// чужой организации.
func (h *Handler) requireOrgRole(w http.ResponseWriter, r *http.Request, orgID, userID int64) (org.Role, bool) {
	role, err := h.Org.Role(r.Context(), orgID, userID)
	if err != nil {
		if errors.Is(err, org.ErrNotMember) {
			h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
			return "", false
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return "", false
	}
	if role != org.RoleOwner && role != org.RoleAdmin {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
		return "", false
	}
	return role, true
}

// hxRedirectWriter перехватывает 303-редирект и превращает его в 200 +
// HX-Redirect, если запрос пришёл от htmx.
type hxRedirectWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *hxRedirectWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if code == http.StatusSeeOther {
		loc := w.Header().Get("Location")
		w.Header().Del("Location")
		w.Header().Set("HX-Redirect", loc)
		w.ResponseWriter.WriteHeader(http.StatusOK)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *hxRedirectWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// sameOrigin — защита POST-ов от CSRF без токенов: Origin (либо, если его
// нет, Referer) обязан совпадать с BaseURL по scheme+host. Обычные формы
// браузер всегда снабжает Origin, поэтому пустые Origin и Referer тоже
// считаются нарушением.
func sameOrigin(r *http.Request, baseURL string) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	return isSameOriginURL(src, baseURL)
}

// isSameOriginURL — совпадает ли raw (Origin/Referer) с baseURL по
// scheme+host. Вынесено из sameOrigin, чтобы им же сверять Referer при
// редиректах после POST (см. bulkRedirectTarget в issues.go).
func isSameOriginURL(raw, baseURL string) bool {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == base.Scheme && u.Host == base.Host
}
