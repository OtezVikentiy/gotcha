package web

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// slosPath — адрес раздела SLO проекта (зеркалит templates.SLOsPath, чтобы
// веб-слой не тянул конкатенацию руками).
func slosPath(projectID int64) string {
	return templates.SLOsPath(projectID)
}

// defaultSLOBurnLongMin/ShortMin — окна burn rate по умолчанию (не выведены в
// форму: форма задаёт только порог). Совпадают с DEFAULT миграции 0072 и с
// дефолтами оценщика (T4): 60-минутное «медленное» и 5-минутное «быстрое» окно.
const (
	defaultSLOBurnLongMin   = 60
	defaultSLOBurnShortMin  = 5
	defaultSLOBurnThreshold = 14.4
)

// maxThresholdMS — потолок порога задержки latency-SLO (1 час в мс). Выше
// бессмысленно для SLI задержки и вдобавок переполняет колонку int4 (>2^31-1 →
// INSERT падает → 500 вместо 422). Валидируем до вставки.
const maxThresholdMS = 3_600_000

// slosPage — GET /projects/{id}/slos: список определений SLO с текущим
// достижением и остатком бюджета + форма создания. Доступ — оператор проекта
// (requireProjectOperator), как metric-alerts (спека 2026-08-08).
func (h *Handler) slosPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.SLO == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderSLOs(w, r, http.StatusOK, projectID, nil, "")
}

// sloFormState — введённые значения формы SLO для возврата при ошибке валидации.
func sloFormState(r *http.Request) templates.FormState {
	f := templates.FormState{}
	for _, name := range []string{
		"name", "sli_kind", "target", "window_days",
		"transaction", "environment", "threshold_ms", "monitor_id", "burn_threshold",
	} {
		if v := r.FormValue(name); v != "" {
			f[name] = v
		}
	}
	return f
}

// renderSLOs отрисовывает страницу. form/errMsg — введённые значения и ошибка
// валидации (открывают модалку с сервера), как renderMetricAlerts.
func (h *Handler) renderSLOs(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	slos, err := h.SLO.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	rows := make([]templates.SLORow, 0, len(slos))
	for _, s := range slos {
		rows = append(rows, h.sloRow(r.Context(), s))
	}
	// Мониторы проекта — для выбора в форме uptime-SLO. Ошибка чтения (или
	// отсутствие Uptime-сервиса на этом стенде) не должна ронять страницу:
	// тогда просто нет выбора, форма подскажет завести монитор.
	var monitors []templates.SLOMonitorOption
	if h.Uptime != nil {
		if ms, err := h.Uptime.List(r.Context(), projectID); err == nil {
			monitors = make([]templates.SLOMonitorOption, 0, len(ms))
			for _, m := range ms {
				monitors = append(monitors, templates.SLOMonitorOption{ID: m.ID, Name: m.Name})
			}
		}
	}
	w.WriteHeader(status)
	_ = templates.SLOsScreen(projectID, rows, monitors, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// sloRow считает достижение и остаток бюджета SLO за его окно через провайдер
// соответствующего типа. Провайдера нет (h.SLOProviders не проведён на стенде)
// либо за окно нет событий (total==0) → HasData=false: страница показывает
// прочерк, а не мнимые 0%. Ошибку провайдера трактуем как «нет данных», а не
// 500: список не должен падать целиком из-за одного SLO без телеметрии.
func (h *Handler) sloRow(ctx context.Context, s slo.SLO) templates.SLORow {
	row := templates.SLORow{
		ID:        s.ID,
		Name:      s.Name,
		Kind:      string(s.Kind),
		TargetPct: s.Target * 100,
	}
	p := h.SLOProviders[s.Kind]
	if p == nil {
		return row
	}
	to := time.Now().UTC()
	from := to.Add(-time.Duration(s.WindowDays) * 24 * time.Hour)
	// Клип окна к горизонту хранения источника: за пределами TTL данных нет,
	// и просить их — лишнее сканирование пустых партиций (0 = хранить вечно).
	if cap := p.RetentionCap(); cap > 0 {
		if earliest := to.Add(-cap); from.Before(earliest) {
			from = earliest
		}
	}
	bs, err := p.Buckets(ctx, s, from, to, time.Hour)
	if err != nil {
		return row
	}
	att, ok := slo.Attainment(bs)
	if !ok {
		return row
	}
	rem, _ := slo.BudgetRemainingFraction(bs, s.Target)
	row.HasData = true
	row.AttainmentPct = att * 100
	row.BudgetRemainingPct = rem * 100
	row.Status = sloStatus(rem)
	return row
}

// sloStatus — статус бюджета по доле остатка: исчерпан (≤0), горит (тонкий
// остаток), здоров. Пороги — грубая визуальная классификация для списка;
// фактическое открытие инцидента решает двухоконный burn rate в оценщике (T4),
// не эти границы.
func sloStatus(remaining float64) string {
	switch {
	case remaining <= 0:
		return "exhausted"
	case remaining <= 0.25:
		return "burning"
	default:
		return "healthy"
	}
}

// sloCreate — POST /projects/{id}/slos: создать определение SLO. Валидация
// зависит от типа SLI. Доступ — оператор проекта.
func (h *Handler) sloCreate(w http.ResponseWriter, r *http.Request) {
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
	if h.SLO == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	fail := func(msgKey string) {
		h.renderSLOs(w, r, http.StatusUnprocessableEntity, projectID, sloFormState(r), i18n.T(r.Context(), msgKey))
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		fail("err.slo.name_required")
		return
	}
	kind := slo.SLIKind(r.FormValue("sli_kind"))
	switch kind {
	case slo.SLIAvailability, slo.SLILatency, slo.SLIUptime:
	default:
		fail("err.slo.kind_invalid")
		return
	}
	// Цель вводится в процентах (0..100, оба конца исключены) → доля (0,1).
	pct, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("target")), 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) || pct <= 0 || pct >= 100 {
		fail("err.slo.target_range")
		return
	}
	target := pct / 100
	windowDays, err := strconv.Atoi(r.FormValue("window_days"))
	if err != nil || windowDays < 1 || windowDays > 90 {
		fail("err.slo.window_range")
		return
	}
	burn := defaultSLOBurnThreshold
	if v := strings.TrimSpace(r.FormValue("burn_threshold")); v != "" {
		burn, err = strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(burn) || math.IsInf(burn, 0) || burn <= 0 {
			fail("err.slo.burn_positive")
			return
		}
	}

	s := slo.SLO{
		ProjectID:     projectID,
		Name:          name,
		Kind:          kind,
		Target:        target,
		WindowDays:    windowDays,
		BurnThreshold: burn,
		BurnLongMin:   defaultSLOBurnLongMin,
		BurnShortMin:  defaultSLOBurnShortMin,
		Enabled:       true,
	}

	switch kind {
	case slo.SLIAvailability:
		s.Transaction = strings.TrimSpace(r.FormValue("transaction"))
		s.Environment = strings.TrimSpace(r.FormValue("environment"))
	case slo.SLILatency:
		s.Transaction = strings.TrimSpace(r.FormValue("transaction"))
		s.Environment = strings.TrimSpace(r.FormValue("environment"))
		thr, err := strconv.Atoi(r.FormValue("threshold_ms"))
		if err != nil || thr <= 0 {
			fail("err.slo.threshold_positive")
			return
		}
		if thr > maxThresholdMS {
			fail("err.slo.threshold_max")
			return
		}
		s.ThresholdMS = thr
	case slo.SLIUptime:
		monitorID, err := strconv.ParseInt(r.FormValue("monitor_id"), 10, 64)
		if err != nil || monitorID <= 0 {
			fail("err.slo.monitor_required")
			return
		}
		if !h.monitorInProject(r.Context(), projectID, monitorID) {
			fail("err.slo.monitor_invalid")
			return
		}
		s.MonitorID = &monitorID
	}

	if _, err := h.SLO.Create(r.Context(), s); err != nil {
		if errors.Is(err, slo.ErrTooManySLOs) {
			fail("err.slo.too_many")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, slosPath(projectID), http.StatusSeeOther)
}

// monitorInProject — принадлежит ли монитор проекту. Проверяем через список
// мониторов проекта, а не Uptime.Get(id): Get вернул бы и чужой монитор,
// раскрыв его существование (тот же existence-oracle, что и в остальных
// гейтах). Uptime не проведён (нет монитор-стенда) → всегда false: uptime-SLO
// без мониторов не заводится.
func (h *Handler) monitorInProject(ctx context.Context, projectID, monitorID int64) bool {
	if h.Uptime == nil {
		return false
	}
	ms, err := h.Uptime.List(ctx, projectID)
	if err != nil {
		return false
	}
	for _, m := range ms {
		if m.ID == monitorID {
			return true
		}
	}
	return false
}

// sloDelete — POST /projects/{id}/slos/{sloID}/delete: удалить определение SLO
// (инциденты уходят каскадом). Двухшаговое подтверждение, как у метрик-алертов.
func (h *Handler) sloDelete(w http.ResponseWriter, r *http.Request) {
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
	if h.SLO == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	sloID, err := strconv.ParseInt(r.PathValue("sloID"), 10, 64)
	if err != nil {
		http.Error(w, "bad slo id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.slo_delete.message", "confirm.delete",
			slosPath(projectID), slosPath(projectID)+"/"+strconv.FormatInt(sloID, 10)+"/delete",
			[]templates.HiddenField{{Name: "slo_id", Value: strconv.FormatInt(sloID, 10)}})
		return
	}
	if err := h.SLO.Delete(r.Context(), projectID, sloID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, slosPath(projectID), http.StatusSeeOther)
}
