package web

import (
	"errors"
	"fmt"
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
	// EnqueueLimited проверяет лимиты активных заявок и вставляет строку
	// атомарно (internal/export/store.go) — раздельные «посчитать → вставить»
	// здесь были гонкой check-then-act под конкурентными запросами (P2,
	// ревью задачи 10): подтверждено эмпирически, 8 параллельных постановок
	// при лимите 3 давали от 3 до 6 успешных вставок.
	if _, err := h.Exports.EnqueueLimited(r.Context(), job, maxActivePerUser, maxActivePerProject); err != nil {
		if errors.Is(err, export.ErrActiveLimitReached) {
			h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "err.export.limit_reached"))
			return
		}
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
