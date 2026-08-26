package web

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// exportsListLimit — потолок списка заявок на странице выгрузок: свежие
// сверху, без пагинации со сдвигом (тот же приём, что issueEventsLimit,
// internal/web/issuedetail.go:27). ByProject НЕ санитизирует limit — сюда
// обязана приходить только эта константа, никогда значение из запроса
// (отрицательное всплывает сырой ошибкой PostgreSQL).
const exportsListLimit = 50

// maxActivePerUser/maxActivePerProject — потолки незавершённых (queued+
// running) заявок: тяжёлая выборка по ClickHouse не должна копиться
// бесконечно ни у одного пользователя, ни у проекта целиком (спека §8).
const (
	maxActivePerUser    = 3
	maxActivePerProject = 10
)

// createRateLimit/createRateWindow — лимит частоты постановки заявок,
// ключ «uid|projectID» (h.exportLimiter, web.go): лимит активных заявок
// выше не ловит того, кто ставит заявку и тут же удаляет её — от этого
// защищает именно ограничение частоты (спека §8).
const (
	createRateLimit  = 10
	createRateWindow = time.Hour
)

// exportsPath — адрес страницы выгрузок проекта. Локальная функция в
// пакете web, а не templates.ExportsPath: страница/кнопки/i18n — отдельная
// задача (E1, задача 11); редиректам этого файла собственный i18n-шаблон
// пути не нужен.
func exportsPath(projectID int64) string {
	return fmt.Sprintf("/projects/%d/exports", projectID)
}

// exportRateLimitKey — ключ лимитера частоты: пользователь+проект, чтобы
// активность одного оператора в одном проекте не била по лимиту его же
// заявок в другом.
func exportRateLimitKey(uid, projectID int64) string {
	return fmt.Sprintf("%d|%d", uid, projectID)
}

// exportFilePath — путь файла заявки на диске: <ExportDir>/<id>.<ext>, тот
// же приём, что и у воркера/джанитора (internal/export/worker.go:206,
// internal/export/janitor.go:127). Единая точка сборки пути здесь — чтобы
// download и delete не могли разъехаться в её написании.
func (h *Handler) exportFilePath(job export.Job) string {
	return filepath.Join(h.ExportDir, fmt.Sprintf("%d.%s", job.ID, job.FileExt))
}

// exportNameSanitizer — всё, что не [a-z0-9], схлопывается в один дефис
// (спека §10: кириллица и пробелы — в дефисы).
var exportNameSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

// exportDownloadFilename — имя файла при скачивании:
// gotcha-<kind>-<project>-<YYYYMMDD-HHMM>.<ext> (спека §10). <project> —
// имя проекта, нормализованное в [a-z0-9-]; пусто после нормализации —
// id проекта.
func exportDownloadFilename(job export.Job, projectName string) string {
	slug := strings.Trim(exportNameSanitizer.ReplaceAllString(strings.ToLower(projectName), "-"), "-")
	if slug == "" {
		slug = strconv.FormatInt(job.ProjectID, 10)
	}
	at := time.Now().UTC()
	if job.FinishedAt != nil {
		at = *job.FinishedAt
	}
	return fmt.Sprintf("gotcha-%s-%s-%s.%s", job.Kind, slug, at.Format("20060102-1504"), job.FileExt)
}

// exportsCreate — POST /projects/{id}/exports: ставит заявку на выгрузку.
//
// Гейты по порядку: same-origin → сессия → существование/доступ к проекту
// (requireProjectOperator) → фича включена (h.Exports != nil, проверяется
// до гейта — тот же порядок, что у alertSuppressionSave) → разбор
// kind/format (неизвестное значение — 422, не паника) → относительный
// период разворачивается В АБСОЛЮТНЫЕ границы ЗДЕСЬ и сейчас (заявка «за
// последние 24 часа», исполненная позже, обязана дать тот же файл, что и
// сразу) → лимит активных заявок → лимит частоты → постановка.
func (h *Handler) exportsCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Exports == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	kind, ok := export.ParseKind(r.PostFormValue("kind"))
	if !ok {
		h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "err.export.invalid_kind"))
		return
	}
	format, ok := export.ParseFormat(r.PostFormValue("format"))
	if !ok {
		h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "err.export.invalid_format"))
		return
	}
	tr := h.resolveTimeRange(w, r, "24h")

	projCount, userCount, err := h.Exports.ActiveCounts(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if projCount >= maxActivePerProject || userCount >= maxActivePerUser {
		h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "err.export.limit_reached"))
		return
	}
	if !h.exportLimiter.Allow(exportRateLimitKey(uid, projectID)) {
		h.renderError(w, r, http.StatusTooManyRequests, i18n.T(r.Context(), "err.export.rate_limited"))
		return
	}

	var scopeIssueID int64
	if v := r.PostFormValue("scope_issue_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			scopeIssueID = id
		}
	}

	job := export.Job{
		ProjectID:    projectID,
		CreatedBy:    uid,
		Kind:         kind,
		Format:       format,
		ScopeIssueID: scopeIssueID,
		Params: export.Params{
			Status:      r.PostFormValue("status"),
			Level:       r.PostFormValue("level"),
			Query:       r.PostFormValue("query"),
			Environment: r.PostFormValue("environment"),
			Sort:        r.PostFormValue("sort"),
			Since:       tr.From,
			Until:       tr.To,
		},
		// include_pii — только от админа/владельца орга (authz.CanManage);
		// от оператора молча игнорируется (спека §8), не отказ: маска по
		// умолчанию безопасна, отказывать здесь нечем не помогает.
		IncludePII: authz.CanManage && r.PostFormValue("include_pii") != "",
	}
	if _, err := h.Exports.Enqueue(r.Context(), job); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.export_requested", 0)
	http.Redirect(w, r, exportsPath(projectID), http.StatusSeeOther)
}

// exportsDownload — GET /projects/{id}/exports/{jobID}/download.
//
// Store.Get/Delete принимают только id, без projectID (Task 2) — сверка
// job.ProjectID с {id} из маршрута ОБЯЗАТЕЛЬНА, иначе чужая заявка из
// другого проекта отдавалась бы по одному jobID (межпроектная утечка).
// Доступ к проекту перепроверяется КАЖДЫЙ раз (requireProjectOperator), а
// не только на постановке — он мог быть отозван после. Чужая заявка (не
// автор и не CanManage) — 404, не 403 (существование заявки не раскрываем).
func (h *Handler) exportsDownload(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Exports == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	job, err := h.Exports.Get(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, export.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if job.ProjectID != projectID {
		h.notFound(w, r)
		return
	}
	if job.CreatedBy != uid && !authz.CanManage {
		h.notFound(w, r)
		return
	}
	if job.Status != export.StatusDone {
		h.notFound(w, r)
		return
	}

	path := h.exportFilePath(job)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Строка есть, файла нет — повод посмотреть в уборку (джанитор
			// либо не туда смотрел, либо кто-то снёс файл руками).
			slog.Warn("exportsDownload: file missing for done job", "job_id", job.ID, "path", path)
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	var projectName string
	if p, err := h.Org.GetProject(r.Context(), projectID); err == nil {
		projectName = p.Name
	}
	filename := exportDownloadFilename(job, projectName)

	w.Header().Set("Content-Type", job.Format.ContentType())
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// http.ServeContent уважает уже выставленный Content-Type (не
	// переопределяет сниффингом) — тот же приём, что и у раздачи агента
	// (internal/web/agentdist.go:130).
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// exportsDelete — POST /projects/{id}/exports/{jobID}/delete. Гейты — те же,
// что у exportsDownload; удаляется только терминальная заявка (Terminal()) —
// у queued/running в этот момент может писаться файл.
//
// Файл удаляется ПЕРВЫМ, строка — ВТОРОЙ: осиротевшую строку видно на
// странице выгрузок (её подберёт следующий проход джанитора), осиротевший
// файл на диске — нет. Терминальность проверяется здесь же, в Go, ДО
// удаления файла: Store.Delete повторяет ту же проверку в SQL, но к
// моменту его вызова файл уже снесён бы, если бы полагаться только на неё.
func (h *Handler) exportsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Exports == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	job, err := h.Exports.Get(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, export.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if job.ProjectID != projectID {
		h.notFound(w, r)
		return
	}
	if job.CreatedBy != uid && !authz.CanManage {
		h.notFound(w, r)
		return
	}
	if !job.Status.Terminal() {
		h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "err.export.not_deletable"))
		return
	}

	path := h.exportFilePath(job)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("exportsDelete: failed to remove file", "job_id", job.ID, "path", path, "err", err)
	}
	if err := h.Exports.Delete(r.Context(), job.ID); err != nil && !errors.Is(err, export.ErrNotDeletable) {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, exportsPath(projectID), http.StatusSeeOther)
}

// exportsPage — GET /projects/{id}/exports: список заявок проекта + форма
// постановки.
func (h *Handler) exportsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Exports == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	jobs, err := h.Exports.ByProject(r.Context(), projectID, exportsListLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.renderExportsPage(w, uid, projectID, authz.CanManage, jobs)
}

// exportRow — заявка, подготовленная для рендера: права на конкретную
// строку посчитаны один раз здесь, а не в шаблоне.
type exportRow struct {
	export.Job
	CanDownload  bool
	CanDelete    bool
	DownloadPath string
	DeletePath   string
}

// exportsPageView — данные шаблона exportsPageTemplate.
type exportsPageView struct {
	CreatePath string
	CanManage  bool
	Jobs       []exportRow
}

// exportsPageTemplate — временный неоформленный рендер страницы выгрузок:
// список заявок и форма постановки, без i18n и дизайн-системы проекта.
// Полноценная страница (templ, кнопки на issues/issuedetail, i18n-ключи
// exports.*) — задача 11 плана E1 («Страница «Выгрузки», кнопки и i18n»),
// тем же приёмом, что и заглушки host detail/settings в web.go (план A1,
// задача 14 → 15/16): здесь — рабочий, но неоформленный код, а не TODO.
var exportsPageTemplate = template.Must(template.New("exports").Parse(`<!doctype html>
<title>Exports</title>
<h1>Exports</h1>
<form method="post" action="{{.CreatePath}}">
<select name="kind"><option value="issues">issues</option><option value="events">events</option></select>
<select name="format"><option value="csv">csv</option><option value="json">json</option><option value="ndjson">ndjson</option></select>
{{if .CanManage}}<label><input type="checkbox" name="include_pii"> include PII</label>{{end}}
<button type="submit">Create</button>
</form>
<table>
<thead><tr><th>ID</th><th>Kind</th><th>Format</th><th>Status</th><th>Rows</th><th>Truncated</th><th>Actions</th></tr></thead>
<tbody>
{{range .Jobs}}<tr>
<td>{{.ID}}</td><td>{{.Kind}}</td><td>{{.Format}}</td><td>{{.Status}}</td><td>{{.RowsWritten}}</td><td>{{if .Truncated}}truncated{{end}}</td>
<td>{{if .CanDownload}}<a href="{{.DownloadPath}}">download</a>{{end}} {{if .CanDelete}}<form method="post" action="{{.DeletePath}}"><button type="submit">delete</button></form>{{end}}</td>
</tr>{{else}}<tr><td colspan="7">no exports yet</td></tr>{{end}}
</tbody>
</table>
`))

// renderExportsPage выполняет exportsPageTemplate — единственное место, где
// считаются CanDownload/CanDelete построчно (owns = автор либо CanManage,
// тот же критерий, что и в exportsDownload/exportsDelete).
func (h *Handler) renderExportsPage(w http.ResponseWriter, uid, projectID int64, canManage bool, jobs []export.Job) {
	rows := make([]exportRow, 0, len(jobs))
	for _, j := range jobs {
		owns := j.CreatedBy == uid || canManage
		rows = append(rows, exportRow{
			Job:          j,
			CanDownload:  owns && j.Status == export.StatusDone,
			CanDelete:    owns && j.Status.Terminal(),
			DownloadPath: fmt.Sprintf("%s/%d/download", exportsPath(projectID), j.ID),
			DeletePath:   fmt.Sprintf("%s/%d/delete", exportsPath(projectID), j.ID),
		})
	}
	view := exportsPageView{CreatePath: exportsPath(projectID), CanManage: canManage, Jobs: rows}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := exportsPageTemplate.Execute(w, view); err != nil {
		slog.Error("exportsPage: render", "err", err)
	}
}
