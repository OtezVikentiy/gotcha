package web

import (
	"context"
	"encoding/json"
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
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Тяжёлая выборка по ClickHouse не должна копиться бесконечно ни у пользователя, ни у проекта.
const (
	maxActivePerUser    = 3
	maxActivePerProject = 10
)

// Лимит частоты ловит того, кто ставит заявку и тут же удаляет её — лимит активных заявок это не поймает.
const (
	createRateLimit  = 10
	createRateWindow = time.Hour
)

// ВСЕГДА константа, никогда значение из query: Store.ByProject/ByProjectForUser не санитизируют
// limit, отрицательное значение всплывает сырой ошибкой PostgreSQL.
const exportsListLimit = 50

func exportsPath(projectID int64) string {
	return templates.ExportsPath(projectID)
}

func exportRateLimitKey(uid, projectID int64) string {
	return fmt.Sprintf("%d|%d", uid, projectID)
}

// Единая точка сборки пути — чтобы download и delete не могли разъехаться с воркером/джанитором.
func (h *Handler) exportFilePath(job export.Job) string {
	return filepath.Join(h.ExportDir, fmt.Sprintf("%d.%s", job.ID, job.FileExt))
}

var exportNameSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

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

// Пустая строка — «любой» (как в issue.Filter), не невалидное значение. Сверка — напрямую с
// issue.IsValidStatus/IsValidLevel, единственным владельцем обоих перечней.
func exportParseStatus(v string) (string, bool) {
	if v == "" || issue.IsValidStatus(v) {
		return v, true
	}
	return "", false
}

func exportParseLevel(v string) (string, bool) {
	if v == "" || issue.IsValidLevel(v) {
		return v, true
	}
	return "", false
}

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
	if !h.parseForm(w, r) {
		return
	}

	kind, ok := export.ParseKind(r.PostFormValue("kind"))
	if !ok {
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.invalid_kind"), exportCreateFormState(r))
		return
	}
	format, ok := export.ParseFormat(r.PostFormValue("format"))
	if !ok {
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.invalid_format"), exportCreateFormState(r))
		return
	}
	status, ok := exportParseStatus(r.PostFormValue("status"))
	if !ok {
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.invalid_status"), exportCreateFormState(r))
		return
	}
	level, ok := exportParseLevel(r.PostFormValue("level"))
	if !ok {
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.invalid_level"), exportCreateFormState(r))
		return
	}

	var scopeIssueID int64
	if v := r.PostFormValue("scope_issue_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			scopeIssueID = id
		}
	}
	// ПЕРЕД лимитами: иначе чужой/несуществующий id жёг бы слот лимита пустой заявкой.
	// ErrNotFound — тот же отказ, что и чужой проект, разбирать разницу незачем.
	if scopeIssueID != 0 {
		it, err := h.Issues.Get(r.Context(), scopeIssueID)
		if err != nil && !errors.Is(err, issue.ErrNotFound) {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if err != nil || it.ProjectID != projectID {
			h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
				i18n.T(r.Context(), "err.export.invalid_scope"), exportCreateFormState(r))
			return
		}
	}

	// Без query — parseTimeRange напрямую, не resolveTimeRange: та при пустом query подставляет
	// период из ЧУЖОЙ cookie другого экрана — заявка «за всё время» тихо ушла бы с чужим диапазоном.
	q := r.URL.Query()
	var tr TimeRange
	if q.Get("period") != "" || q.Get("start") != "" {
		tr = h.resolveTimeRange(w, r, RangeAll)
	} else {
		tr = parseTimeRange(q, RangeAll)
	}

	if !h.exportLimiter.Allow(exportRateLimitKey(uid, projectID)) {
		h.renderExportsPage(w, r, http.StatusTooManyRequests, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.rate_limited"), exportCreateFormState(r))
		return
	}

	job := export.Job{
		ProjectID:    projectID,
		CreatedBy:    uid,
		Kind:         kind,
		Format:       format,
		ScopeIssueID: scopeIssueID,
		Params: export.Params{
			Status:      status,
			Level:       level,
			Query:       r.PostFormValue("query"),
			Environment: r.PostFormValue("environment"),
			Sort:        r.PostFormValue("sort"),
			Since:       tr.From,
			Until:       tr.To,
		},
		// От не-admin молча игнорируется, не 403: маска по умолчанию безопасна, отказывать нечем не помогает.
		IncludePII: authz.CanManage && r.PostFormValue("include_pii") != "",
	}
	// Проверка лимита и вставка — одним атомарным SQL, не раздельно: раздельно это гонка
	// check-then-act (эмпирически: 8 параллельных постановок при лимите 3 давали 3-6 вставок).
	if _, err := h.Exports.EnqueueLimited(r.Context(), job, maxActivePerUser, maxActivePerProject); err != nil {
		if errors.Is(err, export.ErrActiveLimitReached) {
			h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
				i18n.T(r.Context(), "err.export.limit_reached"), exportCreateFormState(r))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.export_requested", 0)
	http.Redirect(w, r, exportsPath(projectID), http.StatusSeeOther)
}

// job.ProjectID обязан совпасть с {id} маршрута — иначе заявка утечёт по чужому jobID.
// доступ проверяется при каждом скачивании; чужая заявка — 404, а не 403.
const exportsMetaQueryParam = "meta"

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
		h.renderError(w, r, http.StatusInternalServerError, "")
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

	if r.URL.Query().Get(exportsMetaQueryParam) == "1" {
		h.exportsServeMeta(w, job)
		return
	}

	path := h.exportFilePath(job)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("exportsDownload: file missing for done job", "job_id", job.ID, "path", path)
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	var projectName string
	if p, err := h.Org.GetProject(r.Context(), projectID); err == nil {
		projectName = p.Name
	}
	filename := exportDownloadFilename(job, projectName)

	w.Header().Set("Content-Type", job.Format.ContentType())
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// http.ServeContent уважает уже выставленный Content-Type — не переопределяет сниффингом.
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// Meta считается заново из job, не хранится отдельным файлом — не может разойтись с записью.
func (h *Handler) exportsServeMeta(w http.ResponseWriter, job export.Job) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(export.BuildMeta(job)); err != nil {
		// Заголовки уже ушли клиенту — код ошибки здесь недостижим, остаётся залогировать и выйти.
		slog.Warn("exportsServeMeta: encode", "job_id", job.ID, "err", err)
	}
}

// Файл удаляется ПЕРВЫМ, строка — ВТОРОЙ: осиротевшую строку видно, её подберёт джанитор;
// осиротевший файл на диске — нет. Терминальность проверяется в Go до удаления файла.
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
		h.renderError(w, r, http.StatusInternalServerError, "")
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
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.not_deletable"), nil)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// CSP default-src 'self' без unsafe-inline не исполняет inline confirm() — двухшаговое
	// подтверждение вместо него; jobID уже в пути, hidden-полей переносить не нужно.
	if r.PostFormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.export_delete.message", "confirm.delete",
			exportsPath(projectID), exportsPath(projectID)+"/"+strconv.FormatInt(job.ID, 10)+"/delete", nil,
			"kind", i18n.T(r.Context(), "exports.kind."+string(job.Kind)))
		return
	}

	path := h.exportFilePath(job)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("exportsDelete: failed to remove file", "job_id", job.ID, "path", path, "err", err)
	}
	if err := h.Exports.Delete(r.Context(), job.ID); err != nil && !errors.Is(err, export.ErrNotDeletable) {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, exportsPath(projectID), http.StatusSeeOther)
}

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
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderExportsPage(w, r, http.StatusOK, projectID, uid, authz, "", nil)
}

func (h *Handler) renderExportsPage(w http.ResponseWriter, r *http.Request, status int, projectID, uid int64, authz projectAuthz, errMsg string, form templates.FormState) {
	// Content-Type явно ДО WriteHeader — иначе автоопределение сниффингом первого Write не сработает.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// h.Exports==nil проверяется после доступа к проекту — саму страницу-объяснение видит
	// только оператор проекта, как и рабочую версию.
	if h.Exports == nil {
		w.WriteHeader(status)
		_ = templates.Exports(projectID, nil, authz.CanManage, h.currentEmail(r), false, errMsg, form).Render(r.Context(), w)
		return
	}

	// Фильтр видимости — в SQL (ByProjectForUser), не постфильтром в Go: иначе
	// exportsListLimit съедался бы чужими строками раньше, чем оператор увидел бы свои.
	var jobs []export.Job
	var err error
	if authz.CanManage {
		jobs, err = h.Exports.ByProject(r.Context(), projectID, exportsListLimit)
	} else {
		jobs, err = h.Exports.ByProjectForUser(r.Context(), projectID, uid, exportsListLimit)
	}
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Email автора — одним батч-запросом на все строки, не по одному на каждую.
	// authorIDs без дублей: у ByProject (админ) авторов может быть много разных.
	authorIDs := make([]int64, 0, len(jobs))
	seenAuthor := make(map[int64]bool, len(jobs))
	for _, j := range jobs {
		if !seenAuthor[j.CreatedBy] {
			seenAuthor[j.CreatedBy] = true
			authorIDs = append(authorIDs, j.CreatedBy)
		}
	}
	authorEmails, err := h.Auth.UserEmails(r.Context(), authorIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	rows := make([]templates.ExportView, len(jobs))
	for i, j := range jobs {
		rows[i] = h.exportViewRow(r.Context(), j, uid, authz, authorEmails[j.CreatedBy])
	}
	w.WriteHeader(status)
	_ = templates.Exports(projectID, rows, authz.CanManage, h.currentEmail(r), true, errMsg, form).Render(r.Context(), w)
}

// include_pii — чекбокс: снятый не попадает в r.Form, поэтому здесь просто наличие ключа, не значение.
func exportCreateFormState(r *http.Request) templates.FormState {
	f := templates.FormState{}
	for _, name := range []string{"kind", "format"} {
		if v := r.PostFormValue(name); v != "" {
			f[name] = v
		}
	}
	if r.PostFormValue("include_pii") != "" {
		f["include_pii"] = "1"
	}
	return f
}

// CanDownload/CanDelete повторяют гейты exportsDownload/exportsDelete буква в букву —
// иначе кнопка вела бы на 404.
func (h *Handler) exportViewRow(ctx context.Context, j export.Job, uid int64, authz projectAuthz, author string) templates.ExportView {
	ownOrManaged := j.CreatedBy == uid || authz.CanManage

	var expiresAt time.Time
	if j.ExpiresAt != nil {
		expiresAt = *j.ExpiresAt
	}
	// Непроверенный FailureReasonKey нельзя отдавать шаблону: i18n.T на неизвестном ключе
	// возвращает сам ключ. StatusFailed — вторая проверка против заявки, вернувшейся в очередь.
	var failureReasonKey string
	if j.Status == export.StatusFailed && export.KnownFailureReasonKey(j.FailureReasonKey) {
		failureReasonKey = j.FailureReasonKey
	}
	// ScopeIssueID/FilterCode/PseudonymMasked идут в шаблон как data-* на той же ячейке, что
	// FilterSummary — получателю не нужно парсить локализованный текст, чтобы достать число или код.
	meta := export.BuildMeta(j)
	return templates.ExportView{
		ID:               j.ID,
		KindLabel:        i18n.T(ctx, "exports.kind."+string(j.Kind)),
		FormatLabel:      i18n.T(ctx, "exports.format."+string(j.Format)),
		FilterSummary:    exportFilterSummary(ctx, j),
		ScopeIssueID:     meta.ScopeIssueID,
		FilterCode:       meta.FilterCode,
		PseudonymMasked:  meta.PseudonymNote != "",
		Status:           string(j.Status),
		Rows:             j.RowsWritten,
		Size:             j.Bytes,
		Truncated:        j.Truncated,
		FailureReasonKey: failureReasonKey,
		IncludePII:       j.IncludePII,
		CreatedAt:        j.CreatedAt,
		ExpiresAt:        expiresAt,
		Author:           author,
		CanDownload:      ownOrManaged && j.Status == export.StatusDone,
		CanDelete:        ownOrManaged && j.Status.Terminal(),
	}
}

// Since==Until==zero — единственный сигнал RangeAll (у любого пресета/custom-диапазона оба
// ненулевые) — период показываем всегда, включая «за всё время», отдельного поля-флага не нужно.
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
	switch {
	case !j.Params.Since.IsZero() && !j.Params.Until.IsZero():
		// humanize.Time с явным UTC, не сырой Format: сводка строится без пояса зрителя.
		parts = append(parts, i18n.Tf(ctx, "exports.summary.period",
			"from", humanize.Time(ctx, j.Params.Since, time.UTC),
			"to", humanize.Time(ctx, j.Params.Until, time.UTC)))
	case j.Params.Since.IsZero() && j.Params.Until.IsZero():
		parts = append(parts, i18n.T(ctx, "exports.summary.period_all"))
	default:
		// Развёрнута только одна граница — заявка ещё не прошла exportsCreate целиком: не
		// показываем период (не «за всё время», это была бы неправда).
	}
	if len(parts) == 0 {
		return i18n.T(ctx, "exports.summary.no_filters")
	}
	return strings.Join(parts, ", ")
}
