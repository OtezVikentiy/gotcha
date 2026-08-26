package web

import (
	"context"
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
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
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

// exportsListLimit — сколько заявок проекта показывается на странице
// «Выгрузки» списком без пагинации со сдвигом (тот же приём, что
// issueEventsLimit в issuedetail.go): фиксированная константа пакета,
// НИКОГДА значение из query-параметра — Store.ByProject не санитизирует
// limit, отрицательное значение всплывает сырой ошибкой PostgreSQL (см.
// докблок ByProject в internal/export/store.go).
const exportsListLimit = 50

// exportsPath — адрес страницы выгрузок проекта: локальный алиас в пакете
// web поверх templates.ExportsPath (тот же приём, что alertSuppressionPath
// в alert_suppression.go) — редиректам этого файла удобнее короткое имя без
// префикса пакета.
func exportsPath(projectID int64) string {
	return templates.ExportsPath(projectID)
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

// exportsPage — GET /projects/{id}/exports: список заявок проекта (новые
// сверху, exportsListLimit штук) + форма постановки. Доступ — оператор
// проекта (requireProjectOperator), как у alert-suppression/escalations
// выше (задача 9/10). limit — ВСЕГДА exportsListLimit, константа пакета:
// Store.ByProject не санитизирует его, отрицательное значение отдало бы
// сырую ошибку PostgreSQL (500) — звать со значением из query здесь нельзя
// (см. докблок exportsListLimit).
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

	rows := make([]templates.ExportView, len(jobs))
	for i, j := range jobs {
		rows[i] = h.exportViewRow(r.Context(), j, uid, authz)
	}
	_ = templates.Exports(projectID, rows, authz.CanManage, h.currentEmail(r)).Render(r.Context(), w)
}

// exportViewRow — Job → ExportView: домен переводится в человекочитаемые
// подписи и видимость кнопок ЗДЕСЬ, не в templ (тот же приём, что
// suppressionParentLabel/suppressionChildLabel в alert_suppression.go).
//
// CanDownload/CanDelete повторяют гейты exportsDownload/exportsDelete
// буква в букву: автор заявки либо CanManage, плюс — для удаления —
// терминальный статус (queued/running ещё пишутся воркером). Показывать
// кнопку, которая поведёт на 404, хуже, чем не показывать её вовсе.
func (h *Handler) exportViewRow(ctx context.Context, j export.Job, uid int64, authz projectAuthz) templates.ExportView {
	var author string
	if email, err := h.Auth.UserEmail(ctx, j.CreatedBy); err == nil {
		author = email
	}
	ownOrManaged := j.CreatedBy == uid || authz.CanManage

	var expiresAt time.Time
	if j.ExpiresAt != nil {
		expiresAt = *j.ExpiresAt
	}
	return templates.ExportView{
		ID:            j.ID,
		KindLabel:     i18n.T(ctx, "exports.kind."+string(j.Kind)),
		FormatLabel:   i18n.T(ctx, "exports.format."+string(j.Format)),
		FilterSummary: exportFilterSummary(ctx, j),
		Status:        string(j.Status),
		Rows:          j.RowsWritten,
		Size:          j.Bytes,
		Truncated:     j.Truncated,
		IncludePII:    j.IncludePII,
		CreatedAt:     j.CreatedAt,
		ExpiresAt:     expiresAt,
		Author:        author,
		CanDownload:   ownOrManaged && j.Status == export.StatusDone,
		CanDelete:     ownOrManaged && j.Status.Terminal(),
	}
}

// exportFilterSummary — человекочитаемая сводка того, что реально сузило
// выборку на момент постановки заявки (пустые Params молча пропускаются, а
// не показываются как «все») — колонка «Фильтры» страницы списка. Период
// показывается всегда: Since/Until разворачиваются из относительного окна
// уже на постановке (exportsCreate), без него заявка «за последние 24 часа»
// была бы неотличима на вид от заявки за всё время.
func exportFilterSummary(ctx context.Context, j export.Job) string {
	var parts []string
	if j.ScopeIssueID != 0 {
		parts = append(parts, i18n.Tf(ctx, "exports.summary.issue", "id", strconv.FormatInt(j.ScopeIssueID, 10)))
	}
	if j.Params.Status != "" {
		parts = append(parts, i18n.T(ctx, "issues.status."+j.Params.Status))
	}
	if j.Params.Level != "" {
		parts = append(parts, i18n.T(ctx, "issues.level."+j.Params.Level))
	}
	if j.Params.Environment != "" {
		parts = append(parts, i18n.Tf(ctx, "exports.summary.environment", "env", j.Params.Environment))
	}
	if j.Params.Query != "" {
		parts = append(parts, i18n.Tf(ctx, "exports.summary.query", "query", j.Params.Query))
	}
	if !j.Params.Since.IsZero() && !j.Params.Until.IsZero() {
		parts = append(parts, i18n.Tf(ctx, "exports.summary.period",
			"from", j.Params.Since.UTC().Format("02.01 15:04"),
			"to", j.Params.Until.UTC().Format("02.01 15:04")))
	}
	if len(parts) == 0 {
		return i18n.T(ctx, "exports.summary.no_filters")
	}
	return strings.Join(parts, ", ")
}
