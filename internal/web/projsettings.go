package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func projectSettingsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/settings"
}

func projectSettingsRenamePath(projectID int64) string {
	return projectSettingsPath(projectID) + "/rename"
}

func projectSettingsKeysPath(projectID int64) string {
	return projectSettingsPath(projectID) + "/keys"
}

func projectSettingsKeysRevokePath(projectID int64) string {
	return projectSettingsKeysPath(projectID) + "/revoke"
}

func projectSettingsPerformancePath(projectID int64) string {
	return projectSettingsPath(projectID) + "/performance"
}

func projectSettingsRegressionsPath(projectID int64) string {
	return projectSettingsPath(projectID) + "/regressions"
}

func projectSettingsDeletePath(projectID int64) string {
	return projectSettingsPath(projectID) + "/delete"
}

// perfFormFromProject строит значения формы «Performance» из сохранённого
// проекта: sample_rate/apdex — как есть, пороги детекторов — через
// trace.ConfigFromJSON (та же функция, что читает детектор; пустой/битый JSON
// даёт дефолты, а не нули). Значения строками — так же их ждёт перерисовка 422.
func perfFormFromProject(p org.Project) templates.PerfSettingsForm {
	cfg, _ := trace.ConfigFromJSON([]byte(p.PerfDetectorConfig))
	return templates.PerfSettingsForm{
		SampleRate:         strconv.FormatFloat(p.TransactionSampleRate, 'g', -1, 64),
		ApdexMS:            strconv.FormatInt(int64(p.ApdexThresholdMS), 10),
		NPlusOneMin:        strconv.Itoa(cfg.NPlusOneMin),
		NPlusOneMinTotalMs: strconv.Itoa(cfg.NPlusOneMinTotalMs),
		SlowDBMs:           strconv.Itoa(cfg.SlowDBMs),
		HTTPFloodMin:       strconv.Itoa(cfg.HTTPFloodMin),
	}
}

// regressionFormFromProject строит значения формы «Регрессии» из сохранённого
// проекта через trace.RegressionConfigFromJSON (та же функция, что читает
// детектор регрессий; пустой/битый JSON даёт дефолты, а не нули). ThresholdPct
// и RecoveryPct хранятся долей (0.25), а в форме показываются процентами (25),
// поэтому домножаем на 100. Полы — как есть. Значения строками — так же их ждёт
// перерисовка 422.
func regressionFormFromProject(p org.Project) templates.RegressionSettingsForm {
	cfg, _ := trace.RegressionConfigFromJSON([]byte(p.PerfRegressionConfig))
	return templates.RegressionSettingsForm{
		ThresholdPct:    formatRegressionPercent(cfg.ThresholdPct),
		RecoveryPct:     formatRegressionPercent(cfg.RecoveryPct),
		WindowMinutes:   strconv.Itoa(cfg.WindowMinutes),
		MinSamples:      strconv.Itoa(cfg.MinSamples),
		DurationFloorMs: formatRegressionFloor(cfg.DurationFloorMs),
		FloorLCP:        formatRegressionFloor(cfg.Floor("lcp")),
		FloorINP:        formatRegressionFloor(cfg.Floor("inp")),
		FloorCLS:        formatRegressionFloor(cfg.Floor("cls")),
		FloorFCP:        formatRegressionFloor(cfg.Floor("fcp")),
		FloorTTFB:       formatRegressionFloor(cfg.Floor("ttfb")),
		Enabled:         cfg.Enabled,
	}
}

// formatRegressionPercent показывает долю (0.25) процентом (25). Точность 'g'/6
// значащих цифр гасит артефакты float (0.10×100 = 10.000000000000002 → «10»),
// сохраняя дробные проценты (12.5) для тех, кто их задал напрямую.
func formatRegressionPercent(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'g', 6, 64)
}

// formatRegressionFloor показывает абсолютный пол метрики как есть (0.05 → «0.05»,
// 200 → «200»).
func formatRegressionFloor(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// parsePathProjectID достаёт projectID из {id} пути /projects/{id}/settings*;
// на невалидный id — 404 (тот же принцип, что и у parsePathOrgID).
func (h *Handler) parsePathProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return 0, false
	}
	return projectID, true
}

// requireProjectRole резолвит projectID -> orgID (org.ProjectOrg) и проверяет
// роль вызывающего в этой организации (requireOrgRole): несуществующий
// проект и недостаточная роль дают одну и ту же стилизованную 404.
func (h *Handler) requireProjectRole(w http.ResponseWriter, r *http.Request, projectID, userID int64) (int64, bool) {
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, false
	}
	if _, ok := h.requireOrgRole(w, r, orgID, userID); !ok {
		return 0, false
	}
	return orgID, true
}

// requireProjectOwner — как requireProjectRole, но owner-only (удаление
// проекта — деструктивное действие, доступное только владельцу организации,
// та же граница, что requireOrgOwner у SSO/удаления орга). Несуществующий
// проект и недостаточная роль дают одну и ту же стилизованную 404.
func (h *Handler) requireProjectOwner(w http.ResponseWriter, r *http.Request, projectID, userID int64) bool {
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return false
	}
	return h.requireOrgOwner(w, r, orgID, userID)
}

// purgeProject досылает удаление телеметрии проекта в ClickHouse после
// PG-удаления. Best-effort: PG-каскад уже отработал, поэтому ошибка (или
// незаданный Purger) не роняет операцию — только логируется, чтобы осиротевшую
// телеметрию можно было добить повторно.
func (h *Handler) purgeProject(ctx context.Context, projectID int64) {
	if h.Purger == nil {
		slog.Warn("purgeProject: Purger not configured, ClickHouse telemetry left in place", "project_id", projectID)
		return
	}
	if err := h.Purger.PurgeProject(ctx, projectID); err != nil {
		slog.Error("purgeProject: failed to purge ClickHouse telemetry", "project_id", projectID, "err", err)
	}
}

func projectSettingsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidName):
		return i18n.T(ctx, "error.project.invalid_name")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// keyBelongsToProject проверяет принадлежность ключа проекту по уже
// загруженному списку KeysForProject — тот же приём, что и findProject: не
// даём отозвать чужой ключ по id (см. projectSettingsKeyRevoke).
func keyBelongsToProject(keys []org.Key, keyID int64) bool {
	for _, k := range keys {
		if k.ID == keyID {
			return true
		}
	}
	return false
}

// projectSettingsPage — GET /projects/{id}/settings: имя, платформа
// (readonly), таблица ключей, DSN текущего живого ключа. Доступ только
// owner/admin организации проекта (requireProjectRole).
func (h *Handler) projectSettingsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderProjectSettings(w, r, http.StatusOK, orgID, projectID, "", nil, nil)
}

// renderProjectSettings — общий рендер: GET-обработчик и все POST в этом
// файле на 422 (то же сообщение на месте, без редиректа — тот же принцип,
// что и renderOrgSettings/renderTeamsPage). orgID уже известен вызывающему
// (requireProjectRole его вернул) — не запрашиваем его заново.
// perfOverride/regOverride != nil означают перерисовку соответствующей формы
// («Performance»/«Регрессии») с уже отправленными (невалидными) значениями, а
// не значениями из БД — так 422 сохраняет ввод пользователя. Остальные POST в
// файле передают nil: их формы (rename/keys) перерисовки этих значений не
// касаются, берём их из проекта.
func (h *Handler) renderProjectSettings(w http.ResponseWriter, r *http.Request, status int, orgID, projectID int64, errMsg string, perfOverride *templates.PerfSettingsForm, regOverride *templates.RegressionSettingsForm) {
	// Отдельного Get-по-id для проекта в org.Service нет — как и в
	// projectSetup, находим проект в списке всех проектов организации
	// (findProject определён в onboarding.go, тот же пакет).
	projects, err := h.Org.ProjectsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	project, ok := findProject(projects, projectID)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	var dsn string
	if publicKey := firstLiveKey(keys); publicKey != "" {
		dsn = buildDSN(h.BaseURL, publicKey, projectID)
	}
	perf := perfFormFromProject(project)
	if perfOverride != nil {
		perf = *perfOverride
	}
	reg := regressionFormFromProject(project)
	if regOverride != nil {
		reg = *regOverride
	}
	w.WriteHeader(status)
	_ = templates.ProjectSettings(project, keys, dsn, errMsg, h.currentEmail(r), perf, reg, h.RetentionDays).Render(r.Context(), w)
}

// projectSettingsRename — POST /projects/{id}/settings/rename: name.
// ErrInvalidName (пустое имя) → 422.
func (h *Handler) projectSettingsRename(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if err := h.Org.RenameProject(r.Context(), projectID, name); err != nil {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, projectSettingsErrorMessage(r.Context(), err), nil, nil)
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsKeyCreate — POST /projects/{id}/settings/keys: выпускает
// новый DSN-ключ проекта.
func (h *Handler) projectSettingsKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if _, err := h.Org.CreateKey(r.Context(), projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsKeyRevoke — POST /projects/{id}/settings/keys/revoke:
// key_id. Ключ должен принадлежать проекту из пути (проверка через
// KeysForProject), иначе 404 — иначе можно было бы по id отозвать чужой ключ.
func (h *Handler) projectSettingsKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	keyID, err := strconv.ParseInt(r.FormValue("key_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad key_id", http.StatusBadRequest)
		return
	}
	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !keyBelongsToProject(keys, keyID) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline onclick="confirm()" — see renderConfirm): без
	// confirmed=yes показываем страницу подтверждения вместо отзыва ключа.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.key_revoke.message", "project.settings.keys.revoke",
			projectSettingsPath(projectID), projectSettingsKeysRevokePath(projectID),
			[]templates.HiddenField{{Name: "key_id", Value: strconv.FormatInt(keyID, 10)}})
		return
	}
	if err := h.Org.RevokeKey(r.Context(), keyID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsPerformance — POST /projects/{id}/settings/performance:
// доля семплирования, порог Apdex и пороги детекторов. Валидация на стороне
// сервера (sample_rate ∈ [0,1], apdex > 0, каждый порог ≥ 1); при ошибке —
// 422 с перерисовкой формы и сохранением отправленных значений. JSON
// детекторов собирается marshal'ом trace.DetectorConfig — его json-теги РОВНО
// те ключи, что читает trace.ConfigFromJSON, поэтому опечатка в ключе
// невозможна (иначе дефолт молча перекрыл бы ввод).
func (h *Handler) projectSettingsPerformance(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Сырые значения формы — их же возвращаем в форму при 422, чтобы не терять
	// ввод пользователя (в т.ч. невалидный, например «1.5»).
	submitted := templates.PerfSettingsForm{
		SampleRate:         r.FormValue("sample_rate"),
		ApdexMS:            r.FormValue("apdex_threshold_ms"),
		NPlusOneMin:        r.FormValue("n_plus_one_min"),
		NPlusOneMinTotalMs: r.FormValue("n_plus_one_min_total_ms"),
		SlowDBMs:           r.FormValue("slow_db_ms"),
		HTTPFloodMin:       r.FormValue("http_flood_min"),
	}
	reject := func(msg string) {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, msg, &submitted, nil)
	}

	sampleRate, err := strconv.ParseFloat(submitted.SampleRate, 64)
	// math.IsNaN отдельно: NaN проходит любое сравнение <0/>1 (все сравнения с
	// NaN ложны), так что без явной проверки «NaN» сохранился бы в колонку.
	if err != nil || math.IsNaN(sampleRate) || sampleRate < 0 || sampleRate > 1 {
		reject(i18n.T(r.Context(), "err.proj.sample_rate"))
		return
	}
	apdexMS, err := strconv.ParseInt(submitted.ApdexMS, 10, 32)
	if err != nil || apdexMS <= 0 {
		reject(i18n.T(r.Context(), "err.proj.apdex"))
		return
	}
	nPlusOneMin, ok1 := parsePerfThreshold(submitted.NPlusOneMin)
	nPlusOneTotal, ok2 := parsePerfThreshold(submitted.NPlusOneMinTotalMs)
	slowDBMs, ok3 := parsePerfThreshold(submitted.SlowDBMs)
	httpFloodMin, ok4 := parsePerfThreshold(submitted.HTTPFloodMin)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		reject(i18n.T(r.Context(), "err.proj.detector_thresholds"))
		return
	}

	cfgJSON, err := json.Marshal(trace.DetectorConfig{
		NPlusOneMin:        nPlusOneMin,
		NPlusOneMinTotalMs: nPlusOneTotal,
		SlowDBMs:           slowDBMs,
		HTTPFloodMin:       httpFloodMin,
	})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if err := h.Org.UpdatePerfSettings(r.Context(), projectID, sampleRate, int32(apdexMS), string(cfgJSON)); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// parsePerfThreshold парсит порог детектора: целое ≥ 1. Ноль/отрицательное
// отвергается на входе — иначе withDefaults молча заменил бы его дефолтом, и
// «0» в форме превратился бы в 500 без объяснений.
func parsePerfThreshold(raw string) (int, bool) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// projectSettingsRegressions — POST /projects/{id}/settings/regressions:
// пороги детектора регрессий. Валидация на стороне сервера: threshold_pct и
// recovery_pct — проценты в (0,100], причём recovery < threshold (гистерезис);
// window_minutes ≥ 1; min_samples ≥ 1; каждый пол ≥ 0. При ошибке — 422 с
// перерисовкой формы и сохранением отправленных значений. JSON собирается
// marshal'ом trace.RegressionConfig — его json-теги РОВНО те ключи, что читает
// trace.RegressionConfigFromJSON, поэтому опечатка в ключе невозможна (иначе
// дефолт молча перекрыл бы ввод). Проценты хранятся долей (25 → 0.25).
func (h *Handler) projectSettingsRegressions(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// Сырые значения формы — их же возвращаем в форму при 422, чтобы не терять
	// ввод пользователя (в т.ч. невалидный). enabled — чекбокс: присутствие
	// поля = включено.
	submitted := templates.RegressionSettingsForm{
		ThresholdPct:    r.FormValue("threshold_pct"),
		RecoveryPct:     r.FormValue("recovery_pct"),
		WindowMinutes:   r.FormValue("window_minutes"),
		MinSamples:      r.FormValue("min_samples"),
		DurationFloorMs: r.FormValue("duration_floor_ms"),
		FloorLCP:        r.FormValue("floor_lcp"),
		FloorINP:        r.FormValue("floor_inp"),
		FloorCLS:        r.FormValue("floor_cls"),
		FloorFCP:        r.FormValue("floor_fcp"),
		FloorTTFB:       r.FormValue("floor_ttfb"),
		Enabled:         r.FormValue("enabled") != "",
	}
	reject := func(msg string) {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, msg, nil, &submitted)
	}

	// Проценты: parseRegressionPercent даёт долю (25 → 0.25) и ловит NaN/диапазон.
	thresholdRatio, ok1 := parseRegressionPercent(submitted.ThresholdPct)
	recoveryRatio, ok2 := parseRegressionPercent(submitted.RecoveryPct)
	if !ok1 || !ok2 {
		reject(i18n.T(r.Context(), "err.proj.pct_range"))
		return
	}
	if recoveryRatio >= thresholdRatio {
		reject(i18n.T(r.Context(), "err.proj.recovery_lt_threshold"))
		return
	}
	windowMinutes, ok3 := parsePerfThreshold(submitted.WindowMinutes)
	minSamples, ok4 := parsePerfThreshold(submitted.MinSamples)
	if !ok3 || !ok4 {
		reject(i18n.T(r.Context(), "err.proj.window_samples"))
		return
	}
	durationFloor, okd := parseRegressionFloor(submitted.DurationFloorMs)
	floorLCP, okl := parseRegressionFloor(submitted.FloorLCP)
	floorINP, oki := parseRegressionFloor(submitted.FloorINP)
	floorCLS, okc := parseRegressionFloor(submitted.FloorCLS)
	floorFCP, okf := parseRegressionFloor(submitted.FloorFCP)
	floorTTFB, okt := parseRegressionFloor(submitted.FloorTTFB)
	if !okd || !okl || !oki || !okc || !okf || !okt {
		reject(i18n.T(r.Context(), "err.proj.metric_floors"))
		return
	}

	cfgJSON, err := json.Marshal(trace.RegressionConfig{
		ThresholdPct:    thresholdRatio,
		RecoveryPct:     recoveryRatio,
		WindowMinutes:   windowMinutes,
		MinSamples:      minSamples,
		DurationFloorMs: durationFloor,
		VitalFloor: map[string]float64{
			"lcp":  floorLCP,
			"inp":  floorINP,
			"cls":  floorCLS,
			"fcp":  floorFCP,
			"ttfb": floorTTFB,
		},
		Enabled: submitted.Enabled,
	})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if err := h.Org.UpdateRegressionConfig(r.Context(), projectID, string(cfgJSON)); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsDelete — POST /projects/{id}/settings/delete: owner-only
// удаление проекта. Сначала PG-удаление (org.DeleteProject, FK ON DELETE
// CASCADE снимает ключи/мониторы/issues и т.д.), затем best-effort очистка
// телеметрии проекта из ClickHouse (h.purgeProject). Успех → 303 на /projects
// (страница проекта больше не существует).
func (h *Handler) projectSettingsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	if !h.requireProjectOwner(w, r, projectID, uid) {
		return
	}
	// Двухшаговое подтверждение (см. projectSettingsKeyRevoke/renderConfirm):
	// без confirmed=yes показываем страницу подтверждения вместо удаления
	// проекта.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.project_delete.message", "project.settings.danger.delete_submit",
			projectSettingsPath(projectID), projectSettingsDeletePath(projectID), nil)
		return
	}
	if err := h.Org.DeleteProject(r.Context(), projectID); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.purgeProject(r.Context(), projectID)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// parseRegressionPercent парсит процент (шаг 1) и возвращает долю: «25» → 0.25.
// Диапазон (0,100] на входе (доля в (0,1]); math.IsNaN отдельно — NaN проходит
// любое сравнение (все сравнения с NaN ложны), так что «NaN» иначе сохранился
// бы в колонку.
func parseRegressionPercent(raw string) (float64, bool) {
	pct, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(pct) || pct <= 0 || pct > 100 {
		return 0, false
	}
	return pct / 100, true
}

// parseRegressionFloor парсит абсолютный пол метрики: число ≥ 0. math.IsNaN
// отдельно — по той же причине, что и в parseRegressionPercent.
func parseRegressionFloor(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v < 0 {
		return 0, false
	}
	return v, true
}
