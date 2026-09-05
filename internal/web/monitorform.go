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

// atoiOrZero — то же соглашение "невалидное значение -> 0", что и formInt
// (alerts.go), но применённое к произвольной строке (токену внутри
// comma-separated списка), а не к целому значению формы по имени поля.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// parseHeaderLines разбирает textarea "Key: Value" по строкам в map.
// Строки без ":" и с пустым ключом молча пропускаются — это вспомогательный
// парсинг формы, а не отдельная точка валидации (итоговую валидность решает
// uptime.Service.Create/Update через ErrInvalidMonitor, в частности лимит "не
// более 20 заголовков").
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

// parseCommaInts разбирает "200,201, 204" в []int. Токен, который не
// парсится как число, превращается в 0 — заведомо невалидный HTTP-код (вне
// диапазона 100..599), поэтому мусорный ввод надёжно приводит к
// ErrInvalidMonitor на стороне uptime.Service, а не молча теряется.
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
	// Стабильный порядок для повторной отрисовки формы — иначе значение
	// textarea прыгало бы между рендерами (map итерируется в случайном
	// порядке).
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

// maskedHeaderValue — плейсхолдер ЗНАЧЕНИЯ заголовка в форме монитора для
// оператора (не canManageProject): имя заголовка остаётся видимым, значение
// (bearer-токен, api-ключ) — нет. Тот же плейсхолдер служит сентинелом
// «оставить прежнее»: оператор, не тронув строку, отправляет её обратно этой
// же маской, и monitorUpdate восстанавливает сохранённое значение (A2b).
const maskedHeaderValue = "****"

// maskHeaderValues заменяет значения заголовков в тексте формы («Key: Value» по
// строкам) на maskedHeaderValue, оставляя имена. Для оператора реальные
// значения не должны попадать в HTML — тот же принцип, что маскировка цели
// канала (mask.go) и обнуление секрета канала (renderAlerts). Строки без ":" и с
// пустым ключом сохраняются как есть (их и так рисует textarea буквально).
// Пустой текст → пустой (у монитора нет заголовков — нечего маскировать).
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

// isBlankOrMasked — значение заголовка, которое оператор «оставил прежним»:
// пустое либо равное маске. parseHeaderLines уже тримит значения, но тримим и
// здесь на случай прямого вызова.
func isBlankOrMasked(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == maskedHeaderValue
}

// parseHTTPConfig нестрого разбирает http-конфиг монитора. Нужен там, где
// заведомо http-config уже прошёл валидацию (merge/exfil в monitorUpdate) —
// ошибки разбора здесь означают пустой конфиг, не отказ.
func parseHTTPConfig(raw json.RawMessage) uptime.HTTPConfig {
	var c uptime.HTTPConfig
	_ = json.Unmarshal(raw, &c)
	return c
}

// mergeKeptHeaders — keep-on-blank для оператора: значение заголовка, пришедшее
// пустым или маской, заменяется прежним сохранённым (plaintext — stored берётся
// из Get, который уже расшифровал). Имена, которых нет в submitted, не
// добавляются (оператор удалил строку — заголовок удаляется); заголовок с
// реальным новым значением остаётся как введён. Заголовок с маской без
// stored-прообраза сюда дойти не должен — вызывающий (monitorUpdate) обязан
// отсечь его через maskedHeaderMissingStored ДО вызова этой функции (находка
// P1-5: иначе буквальная строка "****" легла бы в конфиг как настоящее
// значение). Пустое значение без прообраза — легитимно (новый заголовок без
// значения) и проходит как есть.
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

// maskedHeaderMissingStored ищет среди submitted заголовок, чьё значение —
// буквально маска (maskedHeaderValue, «****»), но одноимённого сохранённого
// значения в stored нет. Такое сочетание не может возникнуть из «оператор
// оставил поле как есть» (тогда имя совпадало бы со stored) — оно возникает,
// когда оператор ПЕРЕИМЕНОВАЛ заголовок с маской (Authorization -> Authz):
// новое имя не находится в stored, а mergeKeptHeaders без этой проверки
// сохранил бы литеральное «****» как настоящее значение заголовка, молча
// похоронив исходный секрет под старым именем (оно исчезло из submitted и
// будет удалено). Возвращает имя первого такого заголовка или "" (нет
// нарушения) — этого достаточно, чтобы отклонить сохранение целиком.
//
// Пустое значение ("") сюда не попадает: пустая новая строка заголовка —
// легитимный ввод («значения ещё нет»), а не воскресшая маска, и не подлежит
// этой проверке (в отличие от isBlankOrMasked, используемой в
// mergeKeptHeaders/keptHeaderWouldRedirect).
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

// keptHeaderWouldRedirect — при смене URL монитора: есть ли среди заголовков
// формы «оставленный прежним» (пустой/маска), чьё сохранённое значение реально
// существует. Только такой заголовок keep-on-blank подставит и уведёт чужой
// секрет на новый адрес (эксфильтрация) — на него и нужен 422 с требованием
// ввести значение заново. Пустой/замаскированный заголовок БЕЗ сохранённого
// прообраза merge оставит пустым (отправлять нечего) — это не вектор, и 422 на
// него был бы ложным (напр. оператор добавил новую пустую строку заголовка,
// заодно сменив URL монитора, у которого секретов и не было).
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

// parseInt64List разбирает набор строковых id (значения чекбоксов "channels")
// в []int64; отдельные нераспознаваемые значения молча пропускаются (наши же
// чекбоксы всегда шлют валидные id — принадлежность проекту всё равно
// перепроверяет uptime.Service.checkChannelsBelongToProject).
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

// monitorFormDefaults — значения формы создания монитора по умолчанию:
// разумный интервал/таймаут/пороги, majority-консенсус, GET без тела,
// dns A-запись, heartbeat с grace 120с (вдвое больше минимально допустимых
// 60с, см. uptime.validateHeartbeatConfig), "local" заранее отмеченным
// регионом.
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

// monitorFormFromMonitor — форма редактирования, предзаполненная уже
// сохранённым монитором: тип фиксирован (m.Kind), поля конкретного типа
// разбираются из m.Config обратно в текстовые представления формы.
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

// monitorFormFromRequest — форма, перерисованная после неудачного POST: все
// поля берутся буквально из отправленного запроса (r.FormValue), а не из
// перепарсенных типов — так значения, которые ввёл пользователь (включая
// заведомо невалидные), остаются на месте, а не исчезают/округляются.
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

// monitorConfigFromRequest строит типизированный конфиг под kind из полей
// запроса и маршалит его в JSON. Собственных ошибок не возвращает — мусорный
// ввод (не-число в порту/коде/grace) превращается в заведомо невалидное
// значение (см. atoiOrZero/parseCommaInts) и отклоняется уже
// uptime.Service.Create/Update через ErrInvalidMonitor; неизвестный kind
// отдаёт пустой объект — validateMonitor проверяет kind раньше, чем читает
// Config, так что до разбора этого мусора дело не доходит.
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

// parseMonitorForm собирает uptime.Monitor + выбранные regions/channels из
// уже распарсенной формы (r.ParseForm должен быть вызван вызывающей
// стороной). enabled передаётся отдельно: создание всегда включает монитор,
// редактирование сохраняет его текущее состояние — форма не даёт менять
// enabled напрямую (для этого есть Pause/Resume на странице монитора).
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

// monitorFormErrorMessage переводит отказ проверки монитора на язык интерфейса.
//
// Раньше сюда подставлялся err.Error(), и над формой висело «монитор: uptime:
// invalid monitor: http url must be a valid http(s) URL»: слово «монитор»
// дважды на двух языках, имя Go-пакета и английская фраза посреди русской
// страницы. Переводить было нечего — причина существовала только строкой,
// собранной для лога.
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
		// Отказ без кода: остаётся общая формулировка, но уже без внутренностей
		// ошибки в тексте для пользователя — они уходят в лог.
		slog.Warn("web: monitor validation error without code", "error", err)
		return i18n.T(ctx, "error.monitor.generic")
	}
	return i18n.T(ctx, "error.monitor.save_failed")
}

// renderMonitorForm — общий рендер формы создания/редактирования: тянет
// свежий список регионов организации и каналов проекта (нужны на каждый
// рендер, включая 422 после неудачного POST — форма без них не сможет
// отрисовать чекбоксы). Форма доступна оператору проекта (requireProjectOperator,
// задача 2), а channelCheckbox рендерит цель канала буквально — каналы тянутся
// через channelsForView (находка B1), ту же единственную дверь, что и у
// renderAlerts (alerts.go): для не-admin (canManage=false) цель маскируется,
// а Secret обнуляется ДО передачи в шаблон, секрет не должен попасть в HTML
// оператора даже неотрендеренным. canManage приходит от вызывающего
// (requireProjectOperator его уже посчитал — находка B4), а не резолвится
// здесь заново отдельным canManageProject.
func (h *Handler) renderMonitorForm(w http.ResponseWriter, r *http.Request, status int, orgID int64, canManage bool, data templates.MonitorFormData, userEmail string) {
	// Все вызывающие уже гасят h.Uptime/h.Alerts == nil своим guard'ом выше по
	// стеку — этот повторяет его на месте разыменования (тот же приём, что и
	// issues.go:181/monitors.go:138), чтобы будущий вызов, забывший свой
	// guard, получил 404, а не панику.
	if h.Uptime == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	regions, err := h.Uptime.Regions(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	channels, err := h.channelsForView(r.Context(), data.ProjectID, canManage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	data.AllRegions = regions
	data.AllChannels = channels
	data.CanManage = canManage
	w.WriteHeader(status)
	_ = templates.MonitorForm(data, userEmail).Render(r.Context(), w)
}

// monitorNewPage — GET /projects/{id}/monitors/new: форма создания монитора,
// доступна оператору проекта (requireProjectOperator, задача 2, спека
// 2026-08-08) — то есть тому же кругу, кто теперь видит кнопку «New monitor»
// в списке.
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
	// renderMonitorForm дереференсит h.Uptime (регионы) и h.Alerts (каналы) —
	// в стендах без этих подсистем 404, а не паника (тот же guard, что и в
	// monitorsList/metricsList).
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

// monitorEditPage — GET /monitors/{id}/edit: та же форма, предзаполненная
// текущим монитором; тип показывается как readonly (шаблон сам решает это по
// IsEdit) — сам POST update тоже не даёт менять kind (см.
// uptime.Service.Update, который перечитывает kind из БД).
func (h *Handler) monitorEditPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// nil-guard до loadAccessibleMonitor (h.Uptime) и renderMonitorForm
	// (h.Uptime + h.Alerts): в стендах без мониторинга — 404, а не паника.
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
	// A2b: значения заголовков http-монитора видит роль operator, а в них —
	// секреты (bearer-токены, api-ключи). Оператору (не owner/admin) маскируем
	// значения ДО шаблона — сырые в HTML не попадают (тот же принцип, что и
	// маскировка каналов в renderMonitorForm, которая теперь использует тот же
	// authz.CanManage — находка B4 убрала второй, независимый поход в БД,
	// который был здесь раньше). Это ЕДИНСТВЕННЫЙ путь, где реальные
	// сохранённые значения заголовков попали бы в форму: 422-перерисовки
	// берут заголовки из запроса самого оператора (его же ввод), а не из БД.
	if m.Kind == uptime.KindHTTP && !authz.CanManage {
		data.HTTPHeaders = maskHeaderValues(data.HTTPHeaders)
		data.HeadersMasked = data.HTTPHeaders != ""
	}
	h.renderMonitorForm(w, r, http.StatusOK, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
}

// monitorCreate — POST /projects/{id}/monitors: sameOrigin +
// requireProjectOperator, парсинг формы в типизированный конфиг,
// ErrInvalidMonitor -> 422 с перерисованной формой (значения сохранены),
// успех -> 303 на страницу нового монитора (heartbeat-монитор показывает URL
// пинга с токеном именно там, см. monitordetail.templ).
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
	// Create дереференсит h.Uptime; renderMonitorForm (ветка 422) — h.Uptime и
	// h.Alerts; renderMonitorDetail (heartbeat) — h.UptimeQuery. Guard после
	// sameOrigin (403 остаётся раньше 404), но до первого обращения к сервисам.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if created.Kind == uptime.KindHeartbeat && created.HeartbeatToken != "" {
		// Сырой heartbeat-токен доступен только сейчас (в БД — sha256). Рендерим
		// деталь с URL пинга один раз, а не редиректим: redirect потерял бы токен
		// и пользователь не получил бы URL для настройки пинга. requireProjectOperator
		// выше уже подтвердил оператора, поэтому canManage=canOperate=true (задача 2:
		// оба флага теперь про оператора).
		h.renderMonitorDetail(w, r, created, true, true)
		return
	}
	http.Redirect(w, r, monitorDetailPath(created.ID), http.StatusSeeOther)
}

// monitorHeartbeatRegenerate — POST /monitors/{id}/heartbeat/regenerate: выдаёт
// новый heartbeat-токен (старый становится недействителен) и показывает новый
// URL пинга один раз. Смысл: токен хранится хешем, поэтому «посмотреть» старый
// URL нельзя — только перевыпустить.
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
	// nil-guard до loadAccessibleMonitor (h.Uptime) и renderMonitorDetail
	// (h.UptimeQuery): в стендах без мониторинга — 404, а не паника.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Монитор зрителю уже доступен (loadAccessibleMonitor) — существование не
	// секрет, поэтому нехватка прав оператора — честный 403 (№72), а 404
	// остаётся содержательной ошибке «этот монитор не heartbeat». Сегодня
	// ветка недостижима — оператор == доступ (canOperateProject ==
	// CanAccessProject) — но граница объявлена явно на случай расхождения
	// предикатов (спека 2026-08-08).
	if !canOperate {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return
	}
	if m.Kind != uptime.KindHeartbeat {
		h.notFound(w, r)
		return
	}
	// Перевыпуск необратим и ломает работающий cron: старый URL перестаёт
	// отвечать сразу, а восстановить его нельзя — в базе только хеш. Это было
	// единственное необратимое действие продукта без подтверждения, при том что
	// у четырнадцати менее болезненных оно есть.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.heartbeat_rotate.message",
			"confirm.heartbeat_rotate.action",
			monitorDetailPath(m.ID), monitorDetailPath(m.ID)+"/heartbeat/regenerate", nil)
		return
	}
	token, err := h.Uptime.RotateHeartbeatToken(r.Context(), m.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	m.HeartbeatToken = token
	// С задачи 2 кнопки Pause/Resume/Edit/Delete на странице детали — тоже
	// операторские (canManage), не только карточка heartbeat-токена
	// (canOperate): Edit-форма теперь тоже requireProjectOperator, так что
	// прятать Edit от оператора незачем. Оба флага наполняются одним и тем же
	// canOperate, посчитанным выше до RotateHeartbeatToken — второй поход в БД
	// (был canManageProject после мутации) больше не нужен, а заодно исчезает
	// и риск потерять свежий URL, если бы этот второй лукап свалился ошибкой
	// уже после успешной ротации (находка ревью задачи 1).
	h.renderMonitorDetail(w, r, m, canOperate, canOperate)
}

// monitorUpdate — POST /monitors/{id}: тот же принцип, что и monitorCreate,
// но kind и enabled берутся из уже сохранённого монитора (форма их не
// присылает/не может менять).
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
	// Update дереференсит h.Uptime; renderMonitorForm (ветка 422) — h.Uptime и
	// h.Alerts. Guard после sameOrigin, до loadAccessibleMonitor.
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

	// A2b: у оператора (не owner/admin) значения заголовков в форме были
	// замаскированы, поэтому пустое/замаскированное значение означает «оставить
	// прежнее», а не «стереть»; и смена URL у монитора с заголовками требует
	// повторного ввода ВСЕХ значений — иначе оператор перецелил бы монитор на
	// свой хост, сохранив чужой токен, и следующая же проверка отправила бы его
	// туда (эксфильтрация). Admin видит реальные значения и не ограничен. m
	// пришёл из Get — значения заголовков в m.Config уже расшифрованы (plaintext).
	// canManage — из того же гейта, что и выше (находка B4), отдельный поход в
	// БД здесь больше не нужен.
	if m.Kind == uptime.KindHTTP && !authz.CanManage {
		stored := parseHTTPConfig(m.Config)
		submitted := parseHTTPConfig(upd.Config)
		// Блок эксфильтрации ДО keep-on-blank: URL сменился, а среди заголовков
		// формы есть «оставленный прежним» (пустой/маска), чей секрет реально
		// хранится — merge подставил бы его и увёл на новый URL → 422. Пустой
		// заголовок без сохранённого прообраза не вектор (merge оставит пустым),
		// на него 422 не нужен. Проверяем ДО merge.
		if submitted.URL != stored.URL && keptHeaderWouldRedirect(submitted.Headers, stored.Headers) {
			data := monitorFormFromRequest(r, m.ProjectID, m.ID, true, m.Kind)
			data.ErrMsg = i18n.T(r.Context(), "error.monitor.header_reentry_required")
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		// P1-5: заголовок с маской, но без сохранённого прообраза под тем же
		// именем — типично переименование ключа с маской (Authorization -> Authz):
		// секрет остался под старым именем (удалится, его нет в submitted), а
		// новое имя merge заполнил бы буквальной строкой «****» как настоящим
		// значением. Не URL-специфично (в отличие от блока эксфильтрации выше) —
		// проверяем независимо от смены URL, ДО merge.
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
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		upd.Config = merged
	}

	if err := h.Uptime.Update(r.Context(), upd, regions, channelIDs); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		if errors.Is(err, uptime.ErrInvalidMonitor) {
			data := monitorFormFromRequest(r, m.ProjectID, m.ID, true, m.Kind)
			data.ErrMsg = monitorFormErrorMessage(r.Context(), err)
			h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, authz.OrgID, authz.CanManage, data, h.currentEmail(r))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, monitorDetailPath(m.ID), http.StatusSeeOther)
}

// heartbeatPingURL/heartbeatCronSnippet — используются MonitorDetail (шаблон
// показывает их только для kind=heartbeat, см. monitordetail.templ). Живут
// здесь (а не в templates), потому что этот файл — единственное место в
// web-пакете, которое уже знает про h.BaseURL применительно к мониторам;
// вынесены в самостоятельные функции ради теста без похода в http.
func heartbeatPingURL(baseURL, token string) string {
	return baseURL + "/uptime/hb/" + token
}

// heartbeatCronSnippet рекомендует -X POST (не голый GET): у GET нет
// семантической разницы для этого пинга, но ссылка вида /uptime/hb/{token}
// регулярно дёргается не человеком — unfurl-ботом мессенджера, антивирусным
// прокси, префетчем браузера, — и все они шлют GET. heartbeatIgnoreReason
// отсеивает такие запросы по заголовкам/User-Agent, но POST дополнительно
// снижает шанс, что чужой автоматический клиент вообще попадёт по этому URL
// (GET остаётся полностью поддержан и документирован — см. internal/docs).
func heartbeatCronSnippet(baseURL, token string, intervalSeconds int) string {
	minutes := intervalSeconds / 60
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("*/%d * * * * curl -fsS -X POST %s >/dev/null", minutes, heartbeatPingURL(baseURL, token))
}
