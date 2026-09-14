package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// строки без ":" и с пустым ключом молча пропускаются — валидность решает uptime.Service.
func parseHeaderLines(text string) map[string]string {
	headers := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		headers[key] = val
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// нечисловой токен превращается в 0 — заведомо невалидный HTTP-код, ведёт к
// ErrInvalidMonitor, а не молча теряется.
func parseCommaInts(text string) []int {
	var out []int
	for _, tok := range strings.Split(text, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out = append(out, atoiOrZero(tok))
	}
	return out
}

func headersToText(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	// стабильный порядок — иначе textarea прыгала бы между рендерами (map итерируется случайно).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+": "+headers[k])
	}
	return strings.Join(lines, "\n")
}

// та же маска служит сентинелом «оставить прежнее»: не тронув строку, оператор
// отправляет её обратно этим значением, и monitorUpdate восстанавливает сохранённое.
const maskedHeaderValue = "****"

func maskHeaderValues(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		if key == "" {
			continue
		}
		lines[i] = key + ": " + maskedHeaderValue
	}
	return strings.Join(lines, "\n")
}

func isBlankOrMasked(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == maskedHeaderValue
}

func parseHTTPConfig(raw json.RawMessage) uptime.HTTPConfig {
	var c uptime.HTTPConfig
	_ = json.Unmarshal(raw, &c)
	return c
}

// заголовок с маской без stored-прообраза сюда дойти не должен — вызывающий обязан
// отсечь его через maskedHeaderMissingStored ДО вызова, иначе «****» ляжет как значение.
func mergeKeptHeaders(submitted, stored map[string]string) map[string]string {
	if len(submitted) == 0 {
		return submitted
	}
	out := make(map[string]string, len(submitted))
	for name, val := range submitted {
		if isBlankOrMasked(val) {
			if prev, ok := stored[name]; ok {
				out[name] = prev
				continue
			}
		}
		out[name] = val
	}
	return out
}

// ловит переименование заголовка с маской (Authorization -> Authz): новое имя не
// совпадает со stored, и mergeKeptHeaders иначе сохранил бы «****» как значение.
func maskedHeaderMissingStored(submitted, stored map[string]string) string {
	for name, val := range submitted {
		if strings.TrimSpace(val) != maskedHeaderValue {
			continue
		}
		if _, ok := stored[name]; !ok {
			return name
		}
	}
	return ""
}

// при смене URL монитора: keep-on-blank заголовок с реальным сохранённым значением
// увёл бы секрет на новый адрес — на это нужен 422 с требованием ввести значение заново.
func keptHeaderWouldRedirect(submitted, stored map[string]string) bool {
	for name, v := range submitted {
		if isBlankOrMasked(v) {
			if prev, ok := stored[name]; ok && prev != "" {
				return true
			}
		}
	}
	return false
}

func intsToText(vals []int) string {
	strs := make([]string, len(vals))
	for i, v := range vals {
		strs[i] = strconv.Itoa(v)
	}
	return strings.Join(strs, ",")
}

func parseInt64List(values []string) []int64 {
	var out []int64
	for _, v := range values {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

func toStringSet(vals []string) map[string]bool {
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set
}

func toInt64Set(vals []int64) map[int64]bool {
	set := make(map[int64]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set
}

// heartbeat grace 120с — вдвое больше минимально допустимых 60с (uptime.validateHeartbeatConfig).
func monitorFormDefaults(projectID int64) templates.MonitorFormData {
	return templates.MonitorFormData{
		ProjectID:             projectID,
		Kind:                  uptime.KindHTTP,
		IntervalSeconds:       "60",
		TimeoutSeconds:        "10",
		FailThreshold:         "1",
		RecoveryThreshold:     "1",
		Retries:               "0",
		Consensus:             string(uptime.ConsensusMajority),
		RemindEveryMinutes:    "0",
		SSLAlertDays:          "14",
		HTTPMethod:            "GET",
		HTTPFollowRedirects:   true,
		DNSRecordType:         "A",
		HeartbeatGraceSeconds: "120",
		SelectedRegions:       map[string]bool{"local": true},
		SelectedChannels:      map[int64]bool{},
	}
}

func monitorFormFromMonitor(m uptime.Monitor) templates.MonitorFormData {
	data := templates.MonitorFormData{
		ProjectID:             m.ProjectID,
		MonitorID:             m.ID,
		IsEdit:                true,
		Kind:                  m.Kind,
		Name:                  m.Name,
		IntervalSeconds:       strconv.Itoa(m.IntervalSeconds),
		TimeoutSeconds:        strconv.Itoa(m.TimeoutSeconds),
		FailThreshold:         strconv.Itoa(m.FailThreshold),
		RecoveryThreshold:     strconv.Itoa(m.RecoveryThreshold),
		Retries:               strconv.Itoa(m.Retries),
		Consensus:             string(m.Consensus),
		RemindEveryMinutes:    strconv.Itoa(m.RemindEveryMinutes),
		SSLAlertDays:          strconv.Itoa(m.SSLAlertDays),
		SelectedRegions:       toStringSet(m.Regions),
		SelectedChannels:      toInt64Set(m.ChannelIDs),
		HTTPMethod:            "GET",
		DNSRecordType:         "A",
		HeartbeatGraceSeconds: "60",
	}
	switch m.Kind {
	case uptime.KindHTTP:
		var c uptime.HTTPConfig
		_ = json.Unmarshal(m.Config, &c)
		data.HTTPMethod = c.Method
		data.HTTPURL = c.URL
		data.HTTPHeaders = headersToText(c.Headers)
		data.HTTPBody = c.Body
		data.HTTPExpectedStatus = intsToText(c.ExpectedStatus)
		data.HTTPBodyContains = c.BodyContains
		data.HTTPBodyNotContains = c.BodyNotContains
		data.HTTPFollowRedirects = c.FollowRedirects
	case uptime.KindTCP:
		var c uptime.TCPConfig
		_ = json.Unmarshal(m.Config, &c)
		data.TCPHost = c.Host
		data.TCPPort = strconv.Itoa(c.Port)
	case uptime.KindDNS:
		var c uptime.DNSConfig
		_ = json.Unmarshal(m.Config, &c)
		data.DNSHostname = c.Hostname
		data.DNSRecordType = c.RecordType
		data.DNSExpectedValue = c.ExpectedValue
	case uptime.KindHeartbeat:
		var c uptime.HeartbeatConfig
		_ = json.Unmarshal(m.Config, &c)
		data.HeartbeatGraceSeconds = strconv.Itoa(c.GraceSeconds)
	}
	return data
}

// поля берутся буквально из r.FormValue, не из перепарсенных типов — заведомо
// невалидный ввод остаётся на месте, а не исчезает/округляется при 422.
func monitorFormFromRequest(r *http.Request, projectID, monitorID int64, isEdit bool, kind uptime.Kind) templates.MonitorFormData {
	return templates.MonitorFormData{
		ProjectID:             projectID,
		MonitorID:             monitorID,
		IsEdit:                isEdit,
		Kind:                  kind,
		Name:                  r.FormValue("name"),
		IntervalSeconds:       r.FormValue("interval_seconds"),
		TimeoutSeconds:        r.FormValue("timeout_seconds"),
		FailThreshold:         r.FormValue("fail_threshold"),
		RecoveryThreshold:     r.FormValue("recovery_threshold"),
		Retries:               r.FormValue("retries"),
		Consensus:             r.FormValue("consensus"),
		RemindEveryMinutes:    r.FormValue("remind_every_minutes"),
		SSLAlertDays:          r.FormValue("ssl_alert_days"),
		SelectedRegions:       toStringSet(r.PostForm["regions"]),
		SelectedChannels:      toInt64Set(parseInt64List(r.PostForm["channels"])),
		HTTPMethod:            r.FormValue("http_method"),
		HTTPURL:               r.FormValue("http_url"),
		HTTPHeaders:           r.FormValue("http_headers"),
		HTTPBody:              r.FormValue("http_body"),
		HTTPExpectedStatus:    r.FormValue("http_expected_status"),
		HTTPBodyContains:      r.FormValue("http_body_contains"),
		HTTPBodyNotContains:   r.FormValue("http_body_not_contains"),
		HTTPFollowRedirects:   formBool(r, "http_follow_redirects"),
		TCPHost:               r.FormValue("tcp_host"),
		TCPPort:               r.FormValue("tcp_port"),
		DNSHostname:           r.FormValue("dns_hostname"),
		DNSRecordType:         r.FormValue("dns_record_type"),
		DNSExpectedValue:      r.FormValue("dns_expected_value"),
		HeartbeatGraceSeconds: r.FormValue("heartbeat_grace_seconds"),
	}
}

// неизвестный kind отдаёт пустой объект — validateMonitor проверяет kind раньше,
// чем читает Config, так что до разбора этого мусора дело не доходит.
func monitorConfigFromRequest(r *http.Request, kind uptime.Kind) json.RawMessage {
	switch kind {
	case uptime.KindHTTP:
		cfg := uptime.HTTPConfig{
			Method:          strings.TrimSpace(r.FormValue("http_method")),
			URL:             strings.TrimSpace(r.FormValue("http_url")),
			Headers:         parseHeaderLines(r.FormValue("http_headers")),
			Body:            r.FormValue("http_body"),
			ExpectedStatus:  parseCommaInts(r.FormValue("http_expected_status")),
			BodyContains:    r.FormValue("http_body_contains"),
			BodyNotContains: r.FormValue("http_body_not_contains"),
			FollowRedirects: formBool(r, "http_follow_redirects"),
		}
		raw, _ := json.Marshal(cfg)
		return raw
	case uptime.KindTCP:
		cfg := uptime.TCPConfig{
			Host: strings.TrimSpace(r.FormValue("tcp_host")),
			Port: atoiOrZero(r.FormValue("tcp_port")),
		}
		raw, _ := json.Marshal(cfg)
		return raw
	case uptime.KindDNS:
		cfg := uptime.DNSConfig{
			Hostname:      strings.TrimSpace(r.FormValue("dns_hostname")),
			RecordType:    strings.TrimSpace(r.FormValue("dns_record_type")),
			ExpectedValue: r.FormValue("dns_expected_value"),
		}
		raw, _ := json.Marshal(cfg)
		return raw
	case uptime.KindHeartbeat:
		cfg := uptime.HeartbeatConfig{GraceSeconds: atoiOrZero(r.FormValue("heartbeat_grace_seconds"))}
		raw, _ := json.Marshal(cfg)
		return raw
	default:
		return json.RawMessage(`{}`)
	}
}

// enabled передаётся отдельно: форма не даёт его менять напрямую (для этого
// есть Pause/Resume на странице монитора).
func parseMonitorForm(r *http.Request, projectID int64, kind uptime.Kind, enabled bool) (uptime.Monitor, []string, []int64) {
	m := uptime.Monitor{
		ProjectID:          projectID,
		Name:               strings.TrimSpace(r.FormValue("name")),
		Kind:               kind,
		Enabled:            enabled,
		IntervalSeconds:    formInt(r, "interval_seconds"),
		TimeoutSeconds:     formInt(r, "timeout_seconds"),
		Config:             monitorConfigFromRequest(r, kind),
		FailThreshold:      formInt(r, "fail_threshold"),
		RecoveryThreshold:  formInt(r, "recovery_threshold"),
		Retries:            formInt(r, "retries"),
		Consensus:          uptime.Consensus(r.FormValue("consensus")),
		RemindEveryMinutes: formInt(r, "remind_every_minutes"),
		SSLAlertDays:       formInt(r, "ssl_alert_days"),
	}
	regions := r.PostForm["regions"]
	channelIDs := parseInt64List(r.PostForm["channels"])
	return m, regions, channelIDs
}

func monitorFormErrorMessage(ctx context.Context, err error) string {
	var ve *uptime.ValidationError
	if errors.As(err, &ve) {
		kv := make([]string, 0, 2*len(ve.Args))
		for k, v := range ve.Args {
			kv = append(kv, k, v)
		}
		return i18n.Tf(ctx, "error.monitor."+ve.Code, kv...)
	}
	if errors.Is(err, uptime.ErrInvalidMonitor) {
		slog.Warn("web: monitor validation error without code", "error", err)
		return i18n.T(ctx, "error.monitor.generic")
	}
	return i18n.T(ctx, "error.monitor.save_failed")
}

// каналы тянутся через channelsForView: для не-admin (canManage=false) цель маскируется
// и Secret обнуляется ДО шаблона — секрет не должен попасть в HTML даже неотрендеренным.
func (h *Handler) renderMonitorForm(w http.ResponseWriter, r *http.Request, status int, orgID int64, canManage bool, data templates.MonitorFormData, userEmail string) {
	if h.Uptime == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	regions, err := h.Uptime.Regions(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	channels, err := h.channelsForView(r.Context(), data.ProjectID, canManage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	data.AllRegions = regions
	data.AllChannels = channels
	data.CanManage = canManage
	data.SecretKeyInsecure = h.SecretKeyInsecure
	w.WriteHeader(status)
	_ = templates.MonitorForm(data, userEmail).Render(r.Context(), w)
}

func (h *Handler) monitorNewPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Uptime == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderMonitorForm(w, r, http.StatusOK, authz.OrgID, authz.CanManage, monitorFormDefaults(projectID), h.currentEmail(r))
}

func (h *Handler) monitorEditPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	authz, ok := h.requireProjectOperator(w, r, m.ProjectID, uid)
	if !ok {
		return
	}
	data := monitorFormFromMonitor(m)
	// единственный путь, где сохранённые значения заголовков попали бы в форму —
	// 422-перерисовки берут заголовки из ввода оператора, не из БД.
	if m.Kind == uptime.KindHTTP && !authz.CanManage {
		data.HTTPHeaders = maskHeaderValues(data.HTTPHeaders)
		data.HeadersMasked = data.HTTPHeaders != ""
	}
	h.renderMonitorForm(w, r, http.StatusOK, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
}

func (h *Handler) monitorCreate(w http.ResponseWriter, r *http.Request) {
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
	if h.Uptime == nil || h.UptimeQuery == nil || h.Alerts == nil {
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

	kind := uptime.Kind(r.FormValue("kind"))
	m, regions, channelIDs := parseMonitorForm(r, projectID, kind, true)

	created, err := h.Uptime.Create(r.Context(), m, regions, channelIDs)
	if err != nil {
		if errors.Is(err, uptime.ErrInvalidMonitor) {
			data := monitorFormFromRequest(r, projectID, 0, false, kind)
			data.ErrMsg = monitorFormErrorMessage(r.Context(), err)
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if created.Kind == uptime.KindHeartbeat && created.HeartbeatToken != "" {
		// сырой токен доступен только сейчас (в БД — sha256): рендерим деталь с URL
		// пинга один раз, а не редиректим — редирект потерял бы токен.
		h.renderMonitorDetail(w, r, created, true)
		return
	}
	http.Redirect(w, r, monitorDetailPath(created.ID), http.StatusSeeOther)
}

// токен хранится хешем, поэтому «посмотреть» старый URL нельзя — только перевыпустить.
func (h *Handler) monitorHeartbeatRegenerate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil || h.UptimeQuery == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	canOperate, err := h.canOperateProject(r.Context(), m.ProjectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	// монитор зрителю уже доступен — нехватка прав оператора это честный 403, не 404
	// (404 остаётся за «этот монитор не heartbeat»).
	if !canOperate {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return
	}
	if m.Kind != uptime.KindHeartbeat {
		h.notFound(w, r)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// перевыпуск необратим: старый URL перестаёт отвечать сразу, восстановить нельзя — в базе только хеш.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.heartbeat_rotate.message",
			"confirm.heartbeat_rotate.action",
			monitorDetailPath(m.ID), monitorDetailPath(m.ID)+"/heartbeat/regenerate", nil)
		return
	}
	token, err := h.Uptime.RotateHeartbeatToken(r.Context(), m.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	m.HeartbeatToken = token
	h.renderMonitorDetail(w, r, m, canOperate)
}

// kind и enabled берутся из уже сохранённого монитора (форма их не присылает/не может менять).
func (h *Handler) monitorUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	authz, ok := h.requireProjectOperator(w, r, m.ProjectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	upd, regions, channelIDs := parseMonitorForm(r, m.ProjectID, m.Kind, m.Enabled)
	upd.ID = m.ID

	if m.Kind == uptime.KindHTTP && !authz.CanManage {
		stored := parseHTTPConfig(m.Config)
		submitted := parseHTTPConfig(upd.Config)
		// URL сменился + оставленный секрет — иначе merge унёс бы его на новый адрес (эксфильтрация).
		if submitted.URL != stored.URL && keptHeaderWouldRedirect(submitted.Headers, stored.Headers) {
			data := monitorFormFromRequest(r, m.ProjectID, m.ID, true, m.Kind)
			data.ErrMsg = i18n.T(r.Context(), "error.monitor.header_reentry_required")
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		// переименование заголовка с маской без прообраза — иначе merge впишет буквальный «****».
		if name := maskedHeaderMissingStored(submitted.Headers, stored.Headers); name != "" {
			data := monitorFormFromRequest(r, m.ProjectID, m.ID, true, m.Kind)
			data.ErrMsg = i18n.Tf(r.Context(), "error.monitor.header_needs_value", "name", name)
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		// keep-on-blank: пустое/замаскированное значение → прежнее сохранённое.
		submitted.Headers = mergeKeptHeaders(submitted.Headers, stored.Headers)
		merged, err := json.Marshal(submitted)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		upd.Config = merged
	}

	if err := h.Uptime.Update(r.Context(), upd, regions, channelIDs); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "")
			return
		}
		if errors.Is(err, uptime.ErrInvalidMonitor) {
			data := monitorFormFromRequest(r, m.ProjectID, m.ID, true, m.Kind)
			data.ErrMsg = monitorFormErrorMessage(r.Context(), err)
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, monitorDetailPath(m.ID), http.StatusSeeOther)
}

func heartbeatPingURL(baseURL, token string) string {
	return baseURL + "/uptime/hb/" + token
}

// -X POST снижает шанс случайного срабатывания от unfurl-ботов/антивирусных прокси,
// которые дёргают такие ссылки GET'ом (GET остаётся полностью поддержан).
func heartbeatCronSnippet(baseURL, token string, intervalSeconds int) string {
	minutes := intervalSeconds / 60
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("*/%d * * * * curl -fsS -X POST %s >/dev/null", minutes, heartbeatPingURL(baseURL, token))
}
