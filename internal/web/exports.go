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
// НИКОГДА значение из query-параметра — ни Store.ByProject, ни
// Store.ByProjectForUser не санитизируют limit, отрицательное значение
// всплывает сырой ошибкой PostgreSQL (см. их докблоки в
// internal/export/store.go).
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

// exportParseStatus/exportParseLevel — сверка status/level формы выгрузки с
// закрытым перечислением (находка ревью соседней задачи, P1): раньше оба
// поля клались в export.Params прямо из PostFormValue без всякой проверки —
// произвольная строка оседала в БД как параметр заявки и затем рендерилась
// через i18n.T("issues.status."+v)/"issues.level."+v на странице списка
// (exportViewRow ниже), где промах ключа в этом продукте отдаёт САМ КЛЮЧ:
// оператор видел бы на странице собственную подставленную строку в виде
// "issues.status.<мусор>", а каждое уникальное значение заодно плодило
// уникальный промах i18n. Пустая строка — «любой», как и в issue.Filter
// (buildIssueFilter трактует "" как отсутствие фильтра по полю) — остаётся
// допустимой, это НЕ то же самое, что недопустимое значение.
//
// Сверка идёт напрямую с issue.IsValidStatus/issue.IsValidLevel — issue
// единственный владелец обоих перечней (internal/issue/query.go), здесь
// собственной копии множества больше нет (была exportValidStatuses/
// exportValidLevels, зеркалившая issue.query.go и bulkActionStatus/
// issues.go — устранено сторожем internal/guards, см. его докблок).
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

// exportsCreate — POST /projects/{id}/exports: ставит заявку на выгрузку.
//
// Гейты по порядку: same-origin → сессия → существование/доступ к проекту
// (requireProjectOperator) → фича включена (h.Exports != nil, проверяется
// до гейта — тот же порядок, что у alertSuppressionSave) → разбор
// kind/format (неизвестное значение — 422, не паника) → scope_issue_id
// принадлежит ТЕКУЩЕМУ проекту (см. ниже) → относительный период
// разворачивается В АБСОЛЮТНЫЕ границы ЗДЕСЬ и сейчас (заявка «за последние
// 24 часа», исполненная позже, обязана дать тот же файл, что и сразу) →
// лимит частоты → лимит активных заявок → постановка.
//
// scope_issue_id — ПЕРЕД лимитом частоты и лимитом активных, не после:
// находка аудита P3-SEC-4 — чужой/несуществующий id раньше доходил до
// EnqueueLimited и жёг оба лимита холостой заявкой, которая молча собрала
// бы пустой файл (StreamForExport фильтрует WHERE project_id = ? AND
// issue_id IN (?) — утечки чужих данных нет, но слот лимита сгорал ни за
// что, а колонка «Фильтры» показывала чужой id). Отклонять его СРАЗУ после
// разбора kind/format, до того как запрос успеет стоить хоть один слот —
// защита в глубину, не первая линия (первая — что StreamForExport и так не
// отдаст чужие данные).
//
// Лимит частоты — ПЕРЕД лимитом активных, а не наоборот: EnqueueLimited
// проверяет лимит активных заявок и вставляет строку ОДНИМ атомарным SQL-
// запросом (см. её докблок ниже) — это и есть чинка check-then-act, которую
// нашло ревью задачи 10 (раздельные «посчитать → вставить» давали гонку под
// конкурентными запросами). Разнести проверку активных заявок на отдельный
// SELECT ДО лимитера значило бы читать её отдельно от INSERT — вернуть тот
// самый check-then-act. Поставить лимитер ПОСЛЕ EnqueueLimited тоже нельзя:
// строка к этому моменту уже вставлена, откатывать её отдельной транзакцией
// ради лимита частоты — накладнее, чем цена нынешнего порядка. Цена: кто
// упёрся в лимит активных заявок (3 на пользователя), всё равно жжёт слот
// из лимита частоты (10/час) — осознанный компромисс, не забытая мелочь.
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
	// status/level — сверка с закрытым перечислением (находка ревью, P1):
	// см. докблок exportParseStatus/exportParseLevel выше. err.export.invalid_kind/
	// invalid_format здесь не подходят по смыслу (речь не о kind/format) —
	// err.export.invalid_status/invalid_level называют, что именно неверно,
	// вместо общего error.action_failed (использовавшегося раньше как
	// временный fallback, пока заводить новый ключ было нельзя).
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
	// Принадлежность scope_issue_id ТЕКУЩЕМУ проекту — см. докблок функции
	// (находка аудита P3-SEC-4). issue.ErrNotFound трактуется так же, как
	// чужой проект: разбираться, существует ли id вообще, снаружи незачем —
	// в обоих случаях ответ один и тот же отказ.
	if scopeIssueID != 0 {
		it, err := h.Issues.Get(r.Context(), scopeIssueID)
		if err != nil && !errors.Is(err, issue.ErrNotFound) {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if err != nil || it.ProjectID != projectID {
			h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
				i18n.T(r.Context(), "err.export.invalid_scope"), exportCreateFormState(r))
			return
		}
	}

	// Дефолт — «за всё время» (RangeAll), как у списка issues (issues.go).
	// Формы issues/issuedetail сами несут явный ?period=/?start=... в action
	// (exportsPathWithRange, templates/exports.templ) — для них резолв идёт
	// через h.resolveTimeRange как обычно. Ручная форма на самой странице
	// «Выгрузки» (exports.templ:exportsForm) своего селектора периода не
	// несёт и явного query не даёт — и здесь h.resolveTimeRange НЕ годится:
	// при отсутствии query она подставляет вместо RangeAll значение из
	// ЧУЖОЙ cookie rangeCookie (её выставляет ЛЮБОЙ другой экран с явным
	// периодом — logs/hosts/metrics/issues), и заявка «за всё время» молча
	// уезжала в «последние 24 часа», потому что час назад пользователь
	// смотрел графики хостов (аудит 2026-08-27, DEDUP-P1 кластер 5). Поэтому
	// без query период резолвится напрямую через parseTimeRange, БЕЗ похода
	// в cookie; при query — тем же resolveTimeRange, что и раньше.
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
			h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
				i18n.T(r.Context(), "err.export.limit_reached"), exportCreateFormState(r))
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
// exportsMetaQueryParam — query-параметр GET .../download, отличающий запрос
// машиночитаемых метаданных заявки (Meta, internal/export/meta.go) от
// запроса самого файла. Отдельного маршрута сознательно не заведено (F5
// контрактной уборки 2026-08-28, CONTRACT-DECISIONS.md): Meta — соседний
// ресурс уже существующего скачивания, а не самостоятельная сущность
// продукта, поэтому у неё те же гейты доступа (авторство/CanManage/
// Status==Done), что и у файла, — без единой строки в authz_map_test.go,
// который знает маршруты по шаблону пути, а не по query. См. докблок Meta
// (meta.go) — там же перечислены остальные два пути доставки (UI/письмо).
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

	if r.URL.Query().Get(exportsMetaQueryParam) == "1" {
		h.exportsServeMeta(w, job)
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

// exportsServeMeta отдаёт export.BuildMeta(job) в JSON — ветка
// exportsDownload при ?meta=1 (см. exportsMetaQueryParam). Meta считается
// заново из уже прочитанного job, а не читается с диска: она чистая функция
// снимка Job (BuildMeta, meta.go), и хранить для неё отдельный файл рядом с
// данными незачем — заодно исключает расхождение с job, если строку заявки
// когда-нибудь поправят вручную.
func (h *Handler) exportsServeMeta(w http.ResponseWriter, job export.Job) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(export.BuildMeta(job)); err != nil {
		// Тело уже могло начать уходить клиенту (заголовки не откладывают
		// запись) — залогировать и выйти, отдельный код ошибки клиенту тут
		// недостижим ровно как и в остальных местах пакета, отдающих поток
		// напрямую в http.ResponseWriter.
		slog.Warn("exportsServeMeta: encode", "job_id", job.ID, "err", err)
	}
}

// exportsDelete — POST /projects/{id}/exports/{jobID}/delete. Гейты — те же,
// что у exportsDownload; удаляется только терминальная заявка (Terminal()) —
// у queued/running в этот момент может писаться файл. Само удаление —
// двухшаговое подтверждение (confirmed=yes), см. комментарий ниже.
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
		h.renderExportsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, authz,
			i18n.T(r.Context(), "err.export.not_deletable"), nil)
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия — тот
	// же приём, что у 11 из 13 файлов с *Delete в internal/web (находка
	// аудита P2-UX-5). jobID уже часть action-пути (маршрут /exports/{jobID}/
	// delete), поэтому в отличие от maintenanceDelete/metricAlertDelete
	// (window_id/rule_id в теле формы) hidden-полей переносить не нужно —
	// тот же случай, что statusPagesDelete.
	if r.PostFormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.export_delete.message", "confirm.delete",
			exportsPath(projectID), exportsPath(projectID)+"/"+strconv.FormatInt(job.ID, 10)+"/delete", nil)
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
// Store.ByProject/ByProjectForUser не санитизируют его, отрицательное
// значение отдало бы сырую ошибку PostgreSQL (500) — звать со значением из
// query здесь нельзя (см. докблок exportsListLimit).
//
// Видимость строк — по спеке §3: свои заявки видит автор, все заявки
// проекта — только админ/владелец орга (authz.CanManage). Фильтр — в SQL
// (Store.ByProjectForUser), не постфильтром в Go: иначе exportsListLimit
// съедался бы чужими строками раньше, чем оператор увидел бы свои
// собственные (ревью веб-части E1, п.2) — та же строка заявки несёт email
// автора и включённость PII, а это ровно тот аудит-сигнал, который спека
// оставляет админу, а не любому оператору проекта.
//
// h.Exports == nil (фича не сконфигурирована на инстансе) проверяется ПОСЛЕ
// requireProjectOperator, не до — саму страницу-объяснение видит только
// оператор проекта, как и рабочую версию (спека E1 §10: «на странице
// выгрузок — объяснение, а не пустая таблица», не «страница видна кому
// попало»); create/download/delete по-прежнему проверяют h.Exports первыми
// и отдают 404 — это не UI, там объяснять нечего (ревью веб-части E1, п.3).
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

// renderExportsPage — тело GET /projects/{id}/exports, вынесенное в
// переиспользуемую функцию (находка аудита P2-UX-4): раньше отказ
// постановки заявки (kind/format невалиден, чужой scope_issue_id, лимит
// частоты/активных заявок) уходил на chromeless ErrorPage — единственное
// место internal/web, где мутирующий POST звал renderError вместо
// перерисовки СВОЕЙ страницы (соседи renderMetricAlerts/аналоги у
// maintenance перерисовывают себя с errMsg и сохранённым form). Теперь
// exportsCreate/exportsDelete зовут ЭТУ функцию с errMsg/form вместо
// renderError — статус ответа (422/429) при этом сохраняется в w.WriteHeader,
// status передаётся явным параметром, а не всегда 200.
//
// authz — из requireProjectOperator вызывающего обработчика: повторный
// поход за ним отсюда не нужен (тот же приём, что и в остальных local-
// re-render хендлерах пакета).
func (h *Handler) renderExportsPage(w http.ResponseWriter, r *http.Request, status int, projectID, uid int64, authz projectAuthz, errMsg string, form templates.FormState) {
	// Content-Type — ЯВНО, до WriteHeader: тот же приём, что в renderError.
	// WriteHeader(status) отправляет заголовки до первой записи тела, из-за
	// чего автоопределение Content-Type сниффингом первого Write не
	// срабатывает — страница ушла бы вовсе без заголовка, включая штатный
	// путь status=200 из exportsPage (он тоже проходит через эту функцию).
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.Exports == nil {
		w.WriteHeader(status)
		_ = templates.Exports(projectID, nil, authz.CanManage, h.currentEmail(r), false, errMsg, form).Render(r.Context(), w)
		return
	}

	var jobs []export.Job
	var err error
	if authz.CanManage {
		jobs, err = h.Exports.ByProject(r.Context(), projectID, exportsListLimit)
	} else {
		jobs, err = h.Exports.ByProjectForUser(r.Context(), projectID, uid, exportsListLimit)
	}
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Email автора — батчем ОДНИМ запросом на все строки (ревью веб-части
	// E1, п.5): раньше exportViewRow звала h.Auth.UserEmail на КАЖДУЮ
	// строку — до exportsListLimit (50) запросов в PG на один рендер
	// страницы. authorIDs собирается без дублей: у не-CanManage все строки
	// и так одного автора (ByProjectForUser), но у админа (ByProject)
	// авторов может быть много разных — дублировать их id в запросе незачем.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	rows := make([]templates.ExportView, len(jobs))
	for i, j := range jobs {
		rows[i] = h.exportViewRow(r.Context(), j, uid, authz, authorEmails[j.CreatedBy])
	}
	w.WriteHeader(status)
	_ = templates.Exports(projectID, rows, authz.CanManage, h.currentEmail(r), true, errMsg, form).Render(r.Context(), w)
}

// exportCreateFormState — введённые значения формы постановки заявки, чтобы
// вернуть их пользователю при отказе (P2-UX-4) — тот же приём, что
// metricRuleFormState (metricalerts.go). include_pii — чекбокс: снятый не
// попадает в r.Form вовсе (см. докблок FormState.Checked), поэтому здесь —
// просто наличие ключа, а не сравнение значения.
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

// exportViewRow — Job → ExportView: домен переводится в человекочитаемые
// подписи и видимость кнопок ЗДЕСЬ, не в templ (тот же приём, что
// suppressionParentLabel/suppressionChildLabel в alert_suppression.go).
// author приходит готовым из батч-выборки exportsPage (h.Auth.UserEmails) —
// не найденный в карте id (отсутствующий пользователь либо ошибка выборки
// молча пропущена вызывающим) отдаёт пустую строку, тот же контракт, что и
// раньше был у прямого h.Auth.UserEmail здесь.
//
// CanDownload/CanDelete повторяют гейты exportsDownload/exportsDelete
// буква в букву: автор заявки либо CanManage, плюс — для удаления —
// терминальный статус (queued/running ещё пишутся воркером). Показывать
// кнопку, которая поведёт на 404, хуже, чем не показывать её вовсе.
func (h *Handler) exportViewRow(ctx context.Context, j export.Job, uid int64, authz projectAuthz, author string) templates.ExportView {
	ownOrManaged := j.CreatedBy == uid || authz.CanManage

	var expiresAt time.Time
	if j.ExpiresAt != nil {
		expiresAt = *j.ExpiresAt
	}
	// failureReasonKey — только для ЗАВЕДОМО известного ключа (P2-UX-2
	// аудита): j.FailureReasonKey приходит из БД как есть (scanJob не
	// проверяет её содержимое, см. докблок Job.FailureReasonKey), а
	// export.KnownFailureReasonKey — единственный источник правды о том,
	// какие ключи вправе там оказаться. Непроверенное значение отдавать
	// шаблону нельзя: exports.templ переводит его через i18n.T(ctx, key)
	// напрямую (тот же приём, что и Status/"exports.status."+Status), а
	// i18n.T() на неизвестном ключе возвращает сам ключ как есть — экран
	// показал бы пользователю технический идентификатор вместо перевода.
	// Условие j.Status == StatusFailed — вторая половина защиты: заявка,
	// вернувшаяся в очередь после Fail() с непустым (по гонке) полем, не
	// должна показать подсказку об отказе, которого для неё ещё не было.
	var failureReasonKey string
	if j.Status == export.StatusFailed && export.KnownFailureReasonKey(j.FailureReasonKey) {
		failureReasonKey = j.FailureReasonKey
	}
	// meta — F5 контрактной уборки 2026-08-28 (CONTRACT-DECISIONS.md):
	// export.BuildMeta(j), тот же снимок Job, что и exportFilterSummary чуть
	// выше использует для человекочитаемой FilterSummary. ScopeIssueID/
	// FilterCode/PseudonymMasked уходят в exports.templ как атрибуты data-*
	// НА ТОЙ ЖЕ ячейке, что и FilterSummary (см. докблок Meta, meta.go, п.2)
	// — получателю не нужно парсить локализованный текст, чтобы достать
	// число или код.
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

// exportFilterSummary — человекочитаемая сводка того, что реально сузило
// выборку на момент постановки заявки (пустые Params молча пропускаются, а
// не показываются как «все») — колонка «Фильтры» страницы списка. Период
// показывается ВСЕГДА, включая «за всё время» (P4 аудита 2026-08-27,
// DEDUP-P1 кластер 5): Since/Until разворачиваются из относительного окна
// уже на постановке (exportsCreate) и без явного показа периода пользователь
// не может отличить на глаз заявку «за последние 24 часа» от заявки «за всё
// время» — то есть узнать, за какой период файл, ему было попросту негде.
// Since==Until==zero — сигнал именно RangeAll (см. parseTimeRange,
// internal/web/timerange.go: единственный путь получить нулевые границы —
// TimeRange{Key: RangeAll}, у любого пресета/custom-диапазона From/To
// заведомо не нулевые), поэтому отдельного поля в Params под флаг «период не
// задан» заводить не пришлось — Since/Until, уже хранящиеся в params jsonb
// (export.Params, internal/export/job.go), несут это различие сами.
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
		// humanize.Time, не сырой Format: страница списка не знает пояса
		// зрителя (сводка строится в exportViewRow без параметра loc, тот же
		// случай, что humanize.Time(ctx, t, time.UTC) в svg.go/svg_slo.go) —
		// UTC с явной подписью пояса, а не голое число без метки.
		parts = append(parts, i18n.Tf(ctx, "exports.summary.period",
			"from", humanize.Time(ctx, j.Params.Since, time.UTC),
			"to", humanize.Time(ctx, j.Params.Until, time.UTC)))
	case j.Params.Since.IsZero() && j.Params.Until.IsZero():
		// ОБЕ границы нулевые — это RangeAll (см. докблок функции), а не
		// «период не задан»: показываем явно, а не молчим.
		parts = append(parts, i18n.T(ctx, "exports.summary.period_all"))
	default:
		// Развёрнут только один конец периода — заявка ещё не прошла
		// exportsCreate целиком, либо баг постановки. Показывать половину
		// диапазона как «за всё время» было бы неверно, а как обычный
		// период — нечем (второй границы нет): период не показываем вовсе,
		// как и раньше до этой правки.
	}
	if len(parts) == 0 {
		return i18n.T(ctx, "exports.summary.no_filters")
	}
	return strings.Join(parts, ", ")
}
