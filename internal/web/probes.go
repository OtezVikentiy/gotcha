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

// max_interval у разных мониторов разный, а проба стучится в центр каждую секунду
// независимо от заданий — фиксированные 5 минут то же самое по смыслу и проще.
const probeOfflineAfter = 5 * time.Minute

const probeFieldMaxLen = 40

func validProbeField(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= probeFieldMaxLen
}

func probeStatus(p uptime.Probe, now time.Time) string {
	switch {
	case p.Revoked:
		return uptime.ProbeStatusRevoked
	case p.LastSeenAt == nil || now.Sub(*p.LastSeenAt) > probeOfflineAfter:
		return uptime.ProbeStatusOffline
	default:
		return uptime.ProbeStatusOnline
	}
}

// Образ назван плейсхолдером, не «gotcha»: публикуемого образа с таким именем нет,
// compose собирает его локально под именем папки — готовая с «gotcha:latest» команда упала бы.
func probeRunCommand(baseURL, token string) string {
	return "docker run -e GOTCHA_PROBE_SERVER_URL=" + baseURL +
		" -e GOTCHA_PROBE_KEY=" + token + " <gotcha-image> --mode=probe"
}

func probeBelongsToOrg(probes []uptime.Probe, probeID int64) bool {
	for _, p := range probes {
		if p.ID == probeID {
			return true
		}
	}
	return false
}

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
	// renderProbes дереференсит h.Uptime.Probes — без подсистемы 404, а не паника.
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	h.renderProbes(w, r, http.StatusOK, orgID, "", "")
}

// Успешный POST рендерит эту же страницу без редиректа: сырой токен пробы нельзя
// протащить через query string или Location, он показывается ровно один раз.
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

func (h *Handler) orgProbesCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	region := strings.TrimSpace(r.FormValue("region"))
	if !validProbeField(name) || !validProbeField(region) {
		h.renderProbes(w, r, http.StatusUnprocessableEntity, orgID,
			i18n.T(r.Context(), "err.probe.name_region"), "")
		return
	}
	// Сравниваем с h.localRegion(), не с константой DefaultRegion: тем именем runner
	// реально лизит (GOTCHA_UPTIME_LOCAL_REGION) — иначе задания молча заберёт LeaseLocal.
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

func (h *Handler) orgProbesRevoke(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireOrgRole(w, r, orgID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	probeID, err := strconv.ParseInt(r.FormValue("probe_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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
	// CSP блокирует inline confirm() — первый POST рендерит страницу подтверждения.
	if r.FormValue("confirmed") != "yes" {
		name := ""
		for _, p := range probes {
			if p.ID == probeID {
				name = p.Name
				break
			}
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.probe_revoke.message", "confirm.revoke",
			orgProbesPath(orgID), orgProbesPath(orgID)+"/revoke",
			[]templates.HiddenField{{Name: "probe_id", Value: strconv.FormatInt(probeID, 10)}},
			"name", name)
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
