package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func orgProbesPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/probes"
}

// probeOfflineAfter — порог, после которого проба считается offline. Спека
// говорит «3 × max_interval», но max_interval у разных мониторов разный, а
// проба стучится в центр каждую секунду независимо от заданий (см.
// uptime.ProbeClient), поэтому фиксированные 5 минут — то же самое по смыслу и
// проще.
const probeOfflineAfter = 5 * time.Minute

// probeFieldMaxLen — предел длины имени и региона пробы: та же валидация, что
// и у регионов монитора (uptime.maxRegionLen) — непусто, не длиннее 40
// символов. Регион пробы попадает в regions монитора один в один, поэтому
// длиннее он быть и не может.
const probeFieldMaxLen = 40

func validProbeField(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= probeFieldMaxLen
}

// probeStatus — статус пробы для таблицы: отозванная — revoked, молчащая
// дольше probeOfflineAfter (или ни разу не стучавшаяся) — offline, иначе
// online.
func probeStatus(p uptime.Probe, now time.Time) string {
	switch {
	case p.Revoked:
		return "revoked"
	case p.LastSeenAt == nil || now.Sub(*p.LastSeenAt) > probeOfflineAfter:
		return "offline"
	default:
		return "online"
	}
}

// probeRunCommand — готовая строка запуска пробы, которую показываем рядом с
// сырым токеном (единственный момент, когда он вообще существует вне БД).
func probeRunCommand(baseURL, token string) string {
	return "docker run -e GOTCHA_SERVER_URL=" + baseURL + " -e GOTCHA_PROBE_TOKEN=" + token + " gotcha --mode=probe"
}

// probeBelongsToOrg проверяет принадлежность пробы организации по уже
// загруженному списку Probes — тот же приём, что и keyBelongsToProject: не
// даём отозвать чужую пробу по id (см. orgProbesRevoke).
func probeBelongsToOrg(probes []uptime.Probe, probeID int64) bool {
	for _, p := range probes {
		if p.ID == probeID {
			return true
		}
	}
	return false
}

// orgProbesPage — GET /orgs/{id}/probes: таблица проб организации (имя,
// регион, статус, last seen, дата создания, кнопка Revoke) и форма создания.
// Доступ только owner/admin (requireOrgRole — та же граница, что и у
// остальных настроек организации).
func (h *Handler) orgProbesPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	h.renderProbes(w, r, http.StatusOK, orgID, "", "")
}

// renderProbes — общий рендер страницы проб: GET, 422 на невалидной форме и
// успешный POST создания. Последний рендерит эту же страницу прямо в теле
// ответа (без редиректа), потому что сырой токен пробы нельзя протащить через
// query string или Location — он показывается ровно один раз, здесь (тот же
// приём, что и ссылка-приглашение в renderOrgSettings).
func (h *Handler) renderProbes(w http.ResponseWriter, r *http.Request, status int, orgID int64, errMsg, rawToken string) {
	o, err := h.Org.Get(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	probes, err := h.Uptime.Probes(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	now := time.Now()
	rows := make([]templates.ProbeRow, 0, len(probes))
	for _, p := range probes {
		rows = append(rows, templates.ProbeRow{Probe: p, Status: probeStatus(p, now)})
	}
	var runCmd string
	if rawToken != "" {
		runCmd = probeRunCommand(h.BaseURL, rawToken)
	}
	w.WriteHeader(status)
	_ = templates.Probes(o, rows, rawToken, runCmd, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// orgProbesCreate — POST /orgs/{id}/probes: name, region. Успех — 200 с той же
// страницей и однократным показом сырого токена (см. renderProbes); пустое или
// слишком длинное имя/регион — 422 с перерисовкой формы.
func (h *Handler) orgProbesCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	region := strings.TrimSpace(r.FormValue("region"))
	if !validProbeField(name) || !validProbeField(region) {
		h.renderProbes(w, r, http.StatusUnprocessableEntity, orgID,
			i18n.T(r.Context(), "err.probe.name_region"), "")
		return
	}
	// Регион встроенной пробы (in-process runner центра) занят. Сравнивать
	// надо именно с h.localRegion() — тем именем, которое runner РЕАЛЬНО
	// лизит (cfg.LocalRegion, GOTCHA_LOCAL_REGION), а не с константой
	// DefaultRegion: при GOTCHA_LOCAL_REGION=eu-central выносную пробу в
	// регионе eu-central завести было бы можно, но её задания забирал бы
	// LeaseLocal центра (он не org-scoped) — монитор проверялся бы из центра,
	// а страница показывала бы регион eu-central.
	if region == h.localRegion() {
		h.renderProbes(w, r, http.StatusUnprocessableEntity, orgID,
			i18n.Tf(r.Context(), "err.probe.region_reserved", "region", h.localRegion()), "")
		return
	}
	_, token, err := h.Uptime.CreateProbe(r.Context(), orgID, region, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.renderProbes(w, r, http.StatusOK, orgID, "", token)
}

// orgProbesRevoke — POST /orgs/{id}/probes/revoke: probe_id. Проба обязана
// принадлежать организации из пути (проверка через Probes), иначе 404 — иначе
// можно было бы по id отозвать чужую пробу. Повторный отзыв уже отозванной
// пробы (uptime.ErrNotFound) — 422 на месте, а не 500.
func (h *Handler) orgProbesRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	orgID, ok := h.parsePathOrgID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	probeID, err := strconv.ParseInt(r.FormValue("probe_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad probe_id", http.StatusBadRequest)
		return
	}
	probes, err := h.Uptime.Probes(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !probeBelongsToOrg(probes, probeID) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	if err := h.Uptime.RevokeProbe(r.Context(), probeID); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderProbes(w, r, http.StatusUnprocessableEntity, orgID, i18n.T(r.Context(), "err.probe.already_revoked"), "")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, orgProbesPath(orgID), http.StatusSeeOther)
}
