package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
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

// ThresholdPct/RecoveryPct хранятся долей (0.25), форма показывает процентами (25).
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
		SeasonalEnabled: cfg.SeasonalEnabled,
		SeasonalWeeks:   strconv.Itoa(cfg.SeasonalWeeks),
	}
}

// 'g'/6 значащих цифр гасит артефакты float (0.10×100 = 10.000000000000002 → «10»).
func formatRegressionPercent(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'g', 6, 64)
}

func formatRegressionFloor(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func (h *Handler) parsePathProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return 0, false
	}
	return projectID, true
}

func (h *Handler) projectOrgOr404(w http.ResponseWriter, r *http.Request, projectID int64) (int64, bool) {
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, false
	}
	return orgID, true
}

func (h *Handler) requireProjectRole(w http.ResponseWriter, r *http.Request, projectID, userID int64) (int64, bool) {
	orgID, ok := h.projectOrgOr404(w, r, projectID)
	if !ok {
		return 0, false
	}
	if _, ok := h.requireOrgRole(w, r, orgID, userID); !ok {
		return 0, false
	}
	return orgID, true
}

func (h *Handler) requireProjectOwner(w http.ResponseWriter, r *http.Request, projectID, userID int64) bool {
	orgID, ok := h.projectOrgOr404(w, r, projectID)
	if !ok {
		return false
	}
	return h.requireOrgOwner(w, r, orgID, userID)
}

func projectSettingsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidName):
		return i18n.T(ctx, "error.project.invalid_name")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

func keyBelongsToProject(keys []org.Key, keyID int64) bool {
	for _, k := range keys {
		if k.ID == keyID {
			return true
		}
	}
	return false
}

func findKey(keys []org.Key, keyID int64) (org.Key, bool) {
	for _, k := range keys {
		if k.ID == keyID {
			return k, true
		}
	}
	return org.Key{}, false
}

// то же усечение (голова 6 / хвост 4), что у keyDisplayID в списке ключей — иначе один
// и тот же ключ выглядел бы на подтверждении и в списке по-разному.
func maskKeyID(publicKey string) string {
	const headRunes, tailRunes = 6, 4
	r := []rune(publicKey)
	if len(r) <= headRunes+tailRunes {
		return publicKey
	}
	return string(r[:headRunes]) + "…" + string(r[len(r)-tailRunes:])
}

func lastLiveKeyOfKind(keys []org.Key, keyID int64) (org.KeyKind, bool) {
	var kind org.KeyKind
	found := false
	for _, k := range keys {
		if k.ID == keyID {
			if k.Revoked {
				return "", false
			}
			kind, found = k.Kind, true
		}
	}
	if !found {
		return "", false
	}
	for _, k := range keys {
		if k.ID != keyID && !k.Revoked && k.Kind == kind {
			return kind, false
		}
	}
	return kind, true
}

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

// perfOverride/regOverride != nil — форма отрисовывается с уже отправленными значениями
// (сохранение ввода при 422), а не значениями из БД.
func (h *Handler) renderProjectSettings(w http.ResponseWriter, r *http.Request, status int, orgID, projectID int64, errMsg string, perfOverride *templates.PerfSettingsForm, regOverride *templates.RegressionSettingsForm) {
	// Отдельного метода get-по-id у org.Service нет — ищем проект в списке всех проектов организации.
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
	views := make([]templates.ProjectKeyView, len(keys))
	for i, k := range keys {
		views[i] = templates.ProjectKeyView{Key: k, DSN: buildDSN(h.BaseURL, k.PublicKey, projectID)}
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
	_ = templates.ProjectSettings(project, views, errMsg, h.currentEmail(r), perf, reg, h.RetentionDays,
		h.deprecatedPathsView(r.Context(), projectID)).Render(r.Context(), w)
}

// 7 дней — порядок величины окна квоты/дропов: реже стучащийся отправитель не должен
// пропасть из виду, если заглянули на следующий день.
const deprecatedIngestPathWindow = 7 * 24 * time.Hour

func (h *Handler) deprecatedPathsView(ctx context.Context, projectID int64) []templates.DeprecatedPathView {
	if h.Signals == nil {
		return nil
	}
	signals, err := h.Signals.ForProject(ctx, projectID)
	if err != nil {
		slog.Warn("deprecatedPathsView: for project", "project_id", projectID, "err", err)
		return nil
	}
	cutoff := time.Now().Add(-deprecatedIngestPathWindow)
	var out []templates.DeprecatedPathView
	for _, sig := range signals {
		path, ok := ingest.PathForDeprecatedKind(sig.Kind)
		if !ok || sig.LastSeenAt.Before(cutoff) {
			continue
		}
		// docs всегда найдётся: путь из PathForDeprecatedKind всегда покрыт ingest.DocsPath.
		docs, _ := ingest.DocsPath(path)
		out = append(out, templates.DeprecatedPathView{Path: string(path), LastSeenAt: sig.LastSeenAt, Hits: sig.Hits, Docs: docs})
	}
	return out
}

func (h *Handler) projectSettingsRename(w http.ResponseWriter, r *http.Request) {
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	name := r.FormValue("name")
	if err := h.Org.RenameProject(r.Context(), projectID, name); err != nil {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, projectSettingsErrorMessage(r.Context(), err), nil, nil)
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) projectSettingsKeyCreate(w http.ResponseWriter, r *http.Request) {
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	kind := org.KeyKind(r.FormValue("kind"))
	// legacy не выпускается через UI: это старый тип ключей, сохранённый только для совместимости.
	if kind == org.KindLegacy || !kind.Valid() {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID,
			i18n.T(r.Context(), "error.key_kind.invalid"), nil, nil)
		return
	}
	if _, err := h.Org.CreateKeys(r.Context(), projectID, kind); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// Ключ должен принадлежать проекту из пути — иначе можно было бы отозвать чужой по id.
func (h *Handler) projectSettingsKeyRevoke(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	keyID, err := strconv.ParseInt(r.FormValue("key_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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
	// Двухшаговое подтверждение: CSP (default-src 'self', без unsafe-inline) не исполняет
	// inline onclick="confirm()", поэтому вместо него отдельная страница подтверждения.
	if r.FormValue("confirmed") != "yes" {
		// Отзыв последнего живого ключа своего типа останавливает приём целого класса
		// телеметрии — предупреждение должно называть это, а не просто спрашивать подтверждение.
		revokedKey, _ := findKey(keys, keyID) // keyBelongsToProject выше уже подтвердила, что ключ найдётся
		msgKey := "confirm.key_revoke.message"
		kv := []string{
			"kind", i18n.T(r.Context(), templates.KeyKindLabelKey(revokedKey)),
			"id", maskKeyID(revokedKey.PublicKey),
		}
		if _, last := lastLiveKeyOfKind(keys, keyID); last {
			msgKey = "confirm.key_revoke.last_of_kind.message"
		}
		h.renderConfirmf(w, r, "confirm.title", msgKey, "project.settings.keys.revoke",
			projectSettingsPath(projectID), projectSettingsKeysRevokePath(projectID),
			[]templates.HiddenField{{Name: "key_id", Value: strconv.FormatInt(keyID, 10)}}, kv...)
		return
	}
	if err := h.Org.RevokeKey(r.Context(), keyID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// json-теги DetectorConfig и ключи, которые читает trace.ConfigFromJSON, — одни и те же:
// опечатка в поле невозможна, дефолт не подменит её молча.
func (h *Handler) projectSettingsPerformance(w http.ResponseWriter, r *http.Request) {
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}

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
	// math.IsNaN отдельно: сравнения с NaN всегда ложны, поэтому без явной проверки «NaN»
	// прошло бы в колонку как валидное значение.
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

// Ноль/отрицательное отвергается на входе — иначе дефолт молча заменил бы его, а «0» в форме
// обернулся бы 500 без объяснений.
func parsePerfThreshold(raw string) (int, bool) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// json-теги RegressionConfig совпадают с ключами, которые читает RegressionConfigFromJSON —
// опечатка в поле невозможна, дефолт не подменит её молча.
func (h *Handler) projectSettingsRegressions(w http.ResponseWriter, r *http.Request) {
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	// enabled — чекбокс: присутствие поля в форме уже означает «включено».
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
		SeasonalEnabled: r.FormValue("seasonal_enabled") != "",
		SeasonalWeeks:   r.FormValue("seasonal_weeks"),
	}
	reject := func(msg string) {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, msg, nil, &submitted)
	}

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
	// Валидируем seasonalWeeks всегда, а не только при seasonal_enabled: форма всегда рендерит
	// число (дефолт 4), так что вне диапазона — ошибка ввода в любом случае.
	seasonalWeeks, err := strconv.Atoi(submitted.SeasonalWeeks)
	if err != nil || seasonalWeeks < 2 || seasonalWeeks > 12 {
		reject(i18n.T(r.Context(), "err.proj.seasonal_weeks"))
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
		Enabled:         submitted.Enabled,
		SeasonalEnabled: submitted.SeasonalEnabled,
		SeasonalWeeks:   seasonalWeeks,
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

// PG-удаление ставит заявку на очистку ClickHouse той же транзакцией; чистит фоновый
// telemetry.PurgeWorker — поэтому ответ про очередь, а не про завершённое удаление.
func (h *Handler) projectSettingsDelete(w http.ResponseWriter, r *http.Request) {
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
	if !h.requireProjectOwner(w, r, projectID, uid) {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// Имя проекта в тексте подтверждения — иначе страница защищает только от случайного клика,
	// а не от удаления не той вкладки/проекта.
	if r.FormValue("confirmed") != "yes" {
		p, err := h.Org.GetProject(r.Context(), projectID)
		if err != nil {
			if errors.Is(err, org.ErrNotFound) {
				h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.project_delete.message", "project.settings.danger.delete_submit",
			projectSettingsPath(projectID), projectSettingsDeletePath(projectID), nil,
			"name", p.Name)
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
	h.flashOK(w, "flash.project_delete_queued", 0)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// math.IsNaN отдельно: сравнения с NaN всегда ложны, «NaN» иначе прошло бы в колонку.
func parseRegressionPercent(raw string) (float64, bool) {
	pct, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(pct) || pct <= 0 || pct > 100 {
		return 0, false
	}
	return pct / 100, true
}

// math.IsNaN отдельно — по той же причине, что и в parseRegressionPercent.
func parseRegressionFloor(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v < 0 {
		return 0, false
	}
	return v, true
}
