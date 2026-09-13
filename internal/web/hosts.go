package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Интерфейс, а не *host.Toucher: Handler.HostForget остаётся nil-safe в стендах без ingest.
type HostForgetter interface {
	Forget(projectID int64, name string)
}

func hostsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/hosts"
}

func hostSettingsPath(projectID int64) string {
	return hostsPath(projectID) + "/settings"
}

func hostDetailPath(projectID int64, name string) string {
	return hostsPath(projectID) + "/" + url.PathEscape(name)
}

func hostDeletePath(projectID int64, name string) string {
	return hostDetailPath(projectID, name) + "/delete"
}

const hostsListWindow = 15 * time.Minute

const hostsListLimit = 500

// Должно совпадать с SQL-условием NewOnly (host.Store.ListFiltered, interval '24 hours') —
// иначе фильтр «новые» и бейдж «новый» у той же строки разъедутся.
const hostNewWindow = 24 * time.Hour

// Проверка Hosts/HostIncidents/HostSettings отдельно от Metrics: некоторые тестовые
// стенды заводят Handler лишь с Metrics, без неё здесь была бы паника, а не 404.
func (h *Handler) hostsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Metrics == nil || h.Hosts == nil || h.HostIncidents == nil || h.HostSettings == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	// Фильтр — строго на стороне SQL, не Go-срезом поверх List: срез поверх уже
	// усечённой лимитом выборки давал бы ложную пустоту вместо совпадений за её пределами.
	q := r.URL.Query()
	filter := host.HostFilter{
		Environment: environmentParam(q),
		Role:        q.Get("role"),
		NewOnly:     q.Get("new") == "1",
	}
	// group — только вью-слой: делит уже отфильтрованные/отсортированные rows на
	// секции, в SQL WHERE не участвует.
	group := normalizeHostGroup(q.Get("group"))

	// +1, чтобы отличить «влезло» от «есть ещё» (truncated ниже), не вычитывая весь реестр.
	hosts, err := h.Hosts.ListFiltered(r.Context(), projectID, filter, hostsListLimit+1)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Фасеты — всегда по полному реестру проекта, не по уже отфильтрованной hosts:
	// иначе выбор одного значения env убрал бы из сайдбара остальные значения env.
	envValues, roleValues, err := h.Hosts.FacetValues(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	settings, settingsExist, err := h.HostSettings.GetWithExists(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// ListOpenByProject, а не ListByProject с лимитом: тот отдаёт последние N ЛЮБОГО
	// статуса, и открытый инцидент мог не попасть в выборку среди закопившихся закрытых.
	incidents, err := h.HostIncidents.ListOpenByProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	openKindsByHost := map[int64][]string{}
	for _, inc := range incidents {
		openKindsByHost[inc.HostID] = append(openKindsByHost[inc.HostID], inc.Kind)
	}

	now := time.Now()
	from := now.Add(-hostsListWindow)

	// CPU busy% = 1 − idle-доля, усреднённая по ядрам; худший диск = max по mountpoint'ам;
	// load/core делится здесь в Go, а не в SQL.
	var (
		idleByHost, memByHost, diskByHost, load5mByHost, coresByHost map[string]float64
		metricsFailed                                                bool
	)
	for _, q := range []struct {
		dst      *map[string]float64
		name     string
		matchers []metric.LabelMatcher
		groupKey string
		agg      string
	}{
		{&idleByHost, hostmetric.CPUUtilization, []metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "idle"}}, "cpu", "avg"},
		{&memByHost, hostmetric.MemoryUtilization, []metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "used"}}, "", ""},
		{&diskByHost, hostmetric.FilesystemUtilization, nil, hostmetric.AttrMountpoint, "max"},
		{&load5mByHost, hostmetric.LoadAvg5m, nil, "", ""},
		{&coresByHost, hostmetric.CPULogicalCount, nil, "", ""},
	} {
		byHost, err := h.Metrics.LatestByHost(r.Context(), projectID, q.name, q.matchers, q.groupKey, q.agg, from, now)
		if err != nil {
			slog.Warn("web: hosts list metrics failed", "project_id", projectID, "metric", q.name, "error", err)
			metricsFailed = true
			break
		}
		*q.dst = byHost
	}

	truncated := len(hosts) > hostsListLimit
	if truncated {
		hosts = hosts[:hostsListLimit]
	}

	// Тот же каскад, что у Evaluator/карточки хоста — иначе бейдж «тишина» и сортировка
	// расходятся с тем, что реально действует на хосте.
	var overrides map[int64]host.ThresholdOverride
	if h.HostOverrides != nil && len(hosts) > 0 {
		ids := make([]int64, len(hosts))
		for i, hst := range hosts {
			ids[i] = hst.ID
		}
		overrides, err = h.HostOverrides.GetForHosts(r.Context(), ids)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	var groups []host.GroupThreshold
	if h.GroupThresholds != nil {
		groups, err = h.GroupThresholds.List(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	resolver := host.ThresholdResolver{
		Project:       settings,
		ProjectExists: settingsExist,
		Groups:        groups,
		Overrides:     overrides,
	}

	rows := make([]templates.HostRowVM, 0, len(hosts))
	for _, hst := range hosts {
		row := templates.HostRowVM{
			Name:        hst.Name,
			LastSeen:    hst.LastSeen,
			Environment: hst.Environment,
			Role:        hst.Role,
			IsNew:       now.Sub(hst.FirstSeen) < hostNewWindow,
		}
		if idle, ok := idleByHost[hst.Name]; ok {
			busy := 1 - idle
			row.CPU = &busy
		}
		if v, ok := memByHost[hst.Name]; ok {
			v := v
			row.Mem = &v
		}
		if v, ok := diskByHost[hst.Name]; ok {
			v := v
			row.Disk = &v
		}
		if load, ok := load5mByHost[hst.Name]; ok {
			if cores, ok := coresByHost[hst.Name]; ok && cores > 0 {
				perCore := load / cores
				row.LoadPerCore = &perCore
			}
		}
		eff := resolver.Effective(hst)
		row.StatusKind, row.OpenKinds = hostRowStatus(openKindsByHost[hst.ID], hst.LastSeen, now, eff.Settings)
		rows = append(rows, row)
	}
	sortHostRows(rows)

	installCmd, config, agentReason := h.hostInstallBlocks(r.Context(), projectID)

	filterVM := templates.HostsFilterVM{
		Environment: filter.Environment,
		Role:        filter.Role,
		NewOnly:     filter.NewOnly,
		Active:      filter.Environment != "" || filter.Role != "" || filter.NewOnly,
		Group:       group,
	}
	facets := templates.NewHostsFacets(r.Context(), projectID, filterVM, envValues, roleValues)
	sections := groupHostRows(r.Context(), rows, group)

	_ = templates.HostsList(projectID, rows, truncated, hostsListLimit, filterVM, facets, sections, installCmd, config, agentReason, h.currentEmail(r), metricsFailed).Render(r.Context(), w)
}

func normalizeHostGroup(v string) string {
	if v == "env" || v == "role" {
		return v
	}
	return ""
}

// Не пересортировывает — делит уже готовый порядок sortHostRows на секции.
func groupHostRows(ctx context.Context, rows []templates.HostRowVM, group string) []templates.HostSection {
	if group != "env" && group != "role" {
		return nil
	}
	byKey := map[string][]templates.HostRowVM{}
	for _, row := range rows {
		key := row.Environment
		if group == "role" {
			key = row.Role
		}
		byKey[key] = append(byKey[key], row)
	}
	labelOf := func(key string) string {
		if key == "" {
			return i18n.T(ctx, "hosts.label.none")
		}
		return key
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return labelOf(keys[i]) < labelOf(keys[j]) })
	sections := make([]templates.HostSection, 0, len(keys))
	for _, key := range keys {
		sections = append(sections, templates.HostSection{Label: labelOf(key), Rows: byKey[key]})
	}
	return sections
}

// kind="silent" исключён из проблемных: иначе бейдж мигал бы "Тихий"→"Тишина" в первые ~60с,
// пока Evaluator ещё не открыл свой incident с тем же kind по last_seen.
func hostRowStatus(openKinds []string, lastSeen, now time.Time, settings host.Settings) (kind string, problemKinds []string) {
	hasSilentIncident := false
	for _, k := range openKinds {
		if k == "silent" {
			hasSilentIncident = true
			continue
		}
		problemKinds = append(problemKinds, k)
	}
	if len(problemKinds) > 0 {
		return "problem", problemKinds
	}
	if hasSilentIncident || (settings.SilentEnabled && now.Sub(lastSeen) > settings.SilentAfter) {
		return "silent", nil
	}
	return "ok", nil
}

func hostRowRank(statusKind string) int {
	switch statusKind {
	case "problem":
		return 0
	case "silent":
		return 1
	default:
		return 2
	}
}

// SliceStable: важен стабильный порядок при повторном рендере тех же данных.
func sortHostRows(rows []templates.HostRowVM) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := hostRowRank(rows[i].StatusKind), hostRowRank(rows[j].StatusKind)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Name < rows[j].Name
	})
}

// exclude_fs_types/exclude_mount_points обязательны: без них snap-squashfs (100% ПО ЗАМЫСЛУ),
// tmpfs и overlay заполняли бы топ занятости и открывали ложный порог диска на первом тике.
const collectorConfigTmpl = `receivers:
  hostmetrics:
    collection_interval: 30s
    scrapers:
      cpu:
        metrics:
          system.cpu.utilization: {enabled: true}
          system.cpu.logical.count: {enabled: true}
      memory:
        metrics:
          system.memory.utilization: {enabled: true}
      filesystem:
        exclude_fs_types:
          match_type: strict
          fs_types: [%s]
        exclude_mount_points:
          match_type: regexp
          mount_points: [%s]
        metrics:
          system.filesystem.utilization: {enabled: true}
      disk: {}
      network: {}
      load: {}
      processes: {}
      system:
        metrics:
          system.uptime: {enabled: true}
processors:
  resourcedetection:
    detectors: [env, system]
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [hostmetrics]
      processors: [resourcedetection, batch]
      exporters: [otlphttp]
`

// Списки исключений ФС — из hostmetric.ExcludedFSTypes/ExcludedMountPrefixes, общих с агентом.
func collectorConfig(baseURL, key string) string {
	fsTypes := strings.Join(hostmetric.ExcludedFSTypes, ", ")
	mountPoints := make([]string, len(hostmetric.ExcludedMountPrefixes))
	for i, p := range hostmetric.ExcludedMountPrefixes {
		mountPoints[i] = "^" + p + ".*"
	}
	return fmt.Sprintf(collectorConfigTmpl, fsTypes, strings.Join(mountPoints, ", "), baseURL, key)
}

// Оба блока используют один и тот же ключ типа agent, а не server: конфиг коллектора несёт
// resourcedetection и тем самым регистрирует хост — единственный тип, которому это разрешено.
func (h *Handler) hostInstallBlocks(ctx context.Context, projectID int64) (installCmd, config, agentReason string) {
	keys, err := h.Org.KeysForProject(ctx, projectID)
	if err != nil {
		return "", "", ""
	}
	key := liveKeyFor(keys, org.KindAgent)
	if key == "" {
		return "", "", ""
	}
	config = collectorConfig(h.BaseURL, key)
	switch {
	case !h.agentDistAvailable():
		return "", config, "dist"
	case !agentBaseURLSecure(h.BaseURL):
		return "", config, "insecure"
	}
	return agentInstallCommand(h.BaseURL, key), config, ""
}

// Продублировано из cmd/gotcha/config.go: тот пакет main, импортировать его сюда нельзя.
func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// http:// на произвольном хосте: команда идёт под root, а SHA256SUMS — тем же MITM-уязвимым
// каналом, что и сам бинарь, сверка сумм не защищает.
func agentBaseURLSecure(baseURL string) bool {
	return strings.HasPrefix(baseURL, "https://") || isLocalBaseURL(baseURL)
}

// `sh -c "$(curl ...)"`, не `curl | sh` — не ради проверки целостности (обе формы уязвимы
// MITM без TLS-пиннинга), просто `$(...)` не исполняет байты потоково по мере получения.
func agentInstallCommand(baseURL, key string) string {
	return fmt.Sprintf(`GOTCHA_AGENT_ENDPOINT=%s GOTCHA_AGENT_INGEST_KEY=%s sh -c "$(curl -fsSL %s/install.sh)"`,
		baseURL, key, baseURL)
}

// Без ключа/endpoint: install.sh, однажды запущенный на хосте, помнит их сам (файл окружения
// юнита) и просто переустанавливает бинарь поверх уже настроенного.
func agentUpdateCommand(baseURL string) string {
	return fmt.Sprintf(`sh -c "$(curl -fsSL %s/install.sh)"`, baseURL)
}

// Суффикс git-описания ("-5-gabcdef-dirty") просто отбрасывается — сравнение идёт по базе X.Y.Z.
func parseSemverBase(s string) (maj, min, pat int, ok bool) {
	s = strings.TrimPrefix(s, "v")
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	parts := strings.Split(s[:i], ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, false
	}
	if min, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, false
	}
	if pat, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, false
	}
	return maj, min, pat, true
}

// Невалидный семвер с любой стороны или агент новее сервера — тоже false: молчим, а не
// показываем ложный или пугающий бейдж.
func agentUpdateAvailable(agentV, serverV string) bool {
	aMaj, aMin, aPat, aOK := parseSemverBase(agentV)
	sMaj, sMin, sPat, sOK := parseSemverBase(serverV)
	if !aOK || !sOK {
		return false
	}
	if aMaj != sMaj {
		return aMaj < sMaj
	}
	if aMin != sMin {
		return aMin < sMin
	}
	return aPat < sPat
}

// Чекбоксы кодируются явно "1"/"0": HTML не шлёт снятый чекбокс, и пустая карта была бы
// неотличима от первого открытия формы.
func hostSettingsFormState(r *http.Request) templates.FormState {
	return templates.FormState{
		"disk_enabled":     boolFormValue(r.FormValue("disk_enabled") != ""),
		"disk_threshold":   r.FormValue("disk_threshold"),
		"memory_enabled":   boolFormValue(r.FormValue("memory_enabled") != ""),
		"memory_threshold": r.FormValue("memory_threshold"),
		"load_enabled":     boolFormValue(r.FormValue("load_enabled") != ""),
		"load_threshold":   r.FormValue("load_threshold"),
		"silent_enabled":   boolFormValue(r.FormValue("silent_enabled") != ""),
		"silent_after":     r.FormValue("silent_after"),
	}
}

func boolFormValue(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ParseFloat пропускает "NaN"/"Inf" без ошибки, а сравнение с NaN через <=/>= всегда false —
// такой порог тихо проходит Validate и никогда не сработает у оценщика.
func invalidThresholdFloat(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0)
}

// Проценты диска/памяти конвертируются в доли здесь: в UI — проценты, хранится долями.
func parseHostSettingsForm(r *http.Request) (host.Settings, error) {
	diskPct, err := strconv.ParseFloat(r.FormValue("disk_threshold"), 64)
	if err != nil {
		return host.Settings{}, fmt.Errorf("%w: %v", host.ErrInvalidDiskThreshold, err)
	}
	if invalidThresholdFloat(diskPct) {
		return host.Settings{}, fmt.Errorf("%w: got %v", host.ErrInvalidDiskThreshold, diskPct)
	}
	memPct, err := strconv.ParseFloat(r.FormValue("memory_threshold"), 64)
	if err != nil {
		return host.Settings{}, fmt.Errorf("%w: %v", host.ErrInvalidMemoryThreshold, err)
	}
	if invalidThresholdFloat(memPct) {
		return host.Settings{}, fmt.Errorf("%w: got %v", host.ErrInvalidMemoryThreshold, memPct)
	}
	load, err := strconv.ParseFloat(r.FormValue("load_threshold"), 64)
	if err != nil {
		return host.Settings{}, fmt.Errorf("%w: %v", host.ErrInvalidLoadThreshold, err)
	}
	if invalidThresholdFloat(load) {
		return host.Settings{}, fmt.Errorf("%w: got %v", host.ErrInvalidLoadThreshold, load)
	}
	silentMinutes, err := strconv.Atoi(r.FormValue("silent_after"))
	if err != nil {
		return host.Settings{}, fmt.Errorf("%w: %v", host.ErrInvalidSilentAfter, err)
	}
	// Граница проверяется ДО перевода в Duration: большие минуты переполняют int64 наносекунд
	// молча, и Validate получил бы уже испорченное отрицательное значение.
	if silentMinutes < 0 || silentMinutes > int(host.MaxSilentAfter/time.Minute) {
		return host.Settings{}, fmt.Errorf("%w: got %d minutes", host.ErrInvalidSilentAfter, silentMinutes)
	}
	return host.Settings{
		DiskEnabled:     r.FormValue("disk_enabled") != "",
		DiskThreshold:   diskPct / 100,
		MemoryEnabled:   r.FormValue("memory_enabled") != "",
		MemoryThreshold: memPct / 100,
		LoadEnabled:     r.FormValue("load_enabled") != "",
		LoadThreshold:   load,
		SilentEnabled:   r.FormValue("silent_enabled") != "",
		SilentAfter:     time.Duration(silentMinutes) * time.Minute,
	}, nil
}

func hostThresholdsFormState(r *http.Request) templates.FormState {
	return templates.FormState{
		"disk_mode":    r.FormValue("disk_mode"),
		"disk_value":   r.FormValue("disk_value"),
		"memory_mode":  r.FormValue("memory_mode"),
		"memory_value": r.FormValue("memory_value"),
		"load_mode":    r.FormValue("load_mode"),
		"load_value":   r.FormValue("load_value"),
		"silent_mode":  r.FormValue("silent_mode"),
		"silent_value": r.FormValue("silent_value"),
	}
}

// "inherit": оба указателя остаются nil — резолвер идёт дальше по каскаду. Любой другой/
// пустой режим тоже трактуется как "inherit".
func parseHostThresholdsForm(r *http.Request) (host.ThresholdOverride, error) {
	var ov host.ThresholdOverride

	switch r.FormValue("disk_mode") {
	case "override":
		pct, err := strconv.ParseFloat(r.FormValue("disk_value"), 64)
		if err != nil {
			return host.ThresholdOverride{}, fmt.Errorf("%w: %v", host.ErrInvalidDiskThreshold, err)
		}
		if invalidThresholdFloat(pct) {
			return host.ThresholdOverride{}, fmt.Errorf("%w: got %v", host.ErrInvalidDiskThreshold, pct)
		}
		frac := pct / 100
		enabled := true
		ov.DiskEnabled, ov.DiskThreshold = &enabled, &frac
	case "off":
		enabled := false
		ov.DiskEnabled = &enabled
	}

	switch r.FormValue("memory_mode") {
	case "override":
		pct, err := strconv.ParseFloat(r.FormValue("memory_value"), 64)
		if err != nil {
			return host.ThresholdOverride{}, fmt.Errorf("%w: %v", host.ErrInvalidMemoryThreshold, err)
		}
		if invalidThresholdFloat(pct) {
			return host.ThresholdOverride{}, fmt.Errorf("%w: got %v", host.ErrInvalidMemoryThreshold, pct)
		}
		frac := pct / 100
		enabled := true
		ov.MemoryEnabled, ov.MemoryThreshold = &enabled, &frac
	case "off":
		enabled := false
		ov.MemoryEnabled = &enabled
	}

	switch r.FormValue("load_mode") {
	case "override":
		v, err := strconv.ParseFloat(r.FormValue("load_value"), 64)
		if err != nil {
			return host.ThresholdOverride{}, fmt.Errorf("%w: %v", host.ErrInvalidLoadThreshold, err)
		}
		if invalidThresholdFloat(v) {
			return host.ThresholdOverride{}, fmt.Errorf("%w: got %v", host.ErrInvalidLoadThreshold, v)
		}
		enabled := true
		ov.LoadEnabled, ov.LoadThreshold = &enabled, &v
	case "off":
		enabled := false
		ov.LoadEnabled = &enabled
	}

	switch r.FormValue("silent_mode") {
	case "override":
		mins, err := strconv.Atoi(r.FormValue("silent_value"))
		if err != nil {
			return host.ThresholdOverride{}, fmt.Errorf("%w: %v", host.ErrInvalidSilentAfter, err)
		}
		// Та же граница ДО перевода в Duration — переполнение int64 на большом числе минут.
		if mins < 0 || mins > int(host.MaxSilentAfter/time.Minute) {
			return host.ThresholdOverride{}, fmt.Errorf("%w: got %d minutes", host.ErrInvalidSilentAfter, mins)
		}
		d := time.Duration(mins) * time.Minute
		enabled := true
		ov.SilentEnabled, ov.SilentAfter = &enabled, &d
	case "off":
		enabled := false
		ov.SilentEnabled = &enabled
	}

	return ov, nil
}

func hostSettingsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, host.ErrInvalidDiskThreshold):
		return i18n.T(ctx, "error.hostsettings.invalid_disk_threshold")
	case errors.Is(err, host.ErrInvalidMemoryThreshold):
		return i18n.T(ctx, "error.hostsettings.invalid_memory_threshold")
	case errors.Is(err, host.ErrInvalidLoadThreshold):
		return i18n.T(ctx, "error.hostsettings.invalid_load_threshold")
	case errors.Is(err, host.ErrInvalidSilentAfter):
		return i18n.T(ctx, "error.hostsettings.invalid_silent_after")
	default:
		return i18n.T(ctx, "error.internal")
	}
}

func (h *Handler) renderHostSettings(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string, groupForm templates.FormState, groupErrMsg string) {
	settings, err := h.HostSettings.Get(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// nil-safe: main.go проводит их вместе с HostSettings, но тестовые Handler'ы иногда собраны
	// частично — секция тогда просто не покажет правил.
	var groups []host.GroupThreshold
	var envValues, roleValues []string
	if h.Hosts != nil && h.GroupThresholds != nil {
		groups, err = h.GroupThresholds.List(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		envValues, roleValues, err = h.Hosts.FacetValues(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	// Существование правила с этой парой scope+label и есть признак «правка»: Upsert идемпотентен
	// по паре, отдельного маркера формы нет.
	if groupForm != nil {
		scope := groupForm.Get("scope", "")
		label := groupForm.Get("label_env", "")
		if scope == "role" {
			label = groupForm.Get("label_role", "")
		}
		if groupThresholdExists(groups, scope, label) {
			groupForm = groupForm.Open(templates.EditGroupThresholdModalID(scope, label))
		} else {
			groupForm = groupForm.Open(templates.GroupThresholdCreateModalID)
		}
	} else if scope, label := r.URL.Query().Get("gt_scope"), r.URL.Query().Get("gt_label"); groupThresholdExists(groups, scope, label) {
		groupForm = templates.FormState{}.Open(templates.EditGroupThresholdModalID(scope, label))
	}
	installCmd, config, agentReason := h.hostInstallBlocks(r.Context(), projectID)
	w.WriteHeader(status)
	_ = templates.HostSettings(projectID, settings, installCmd, config, agentReason, form, errMsg,
		templates.HostGroupThresholdsVM{
			Groups: groups,
			Envs:   envValues,
			Roles:  roleValues,
			Form:   groupForm,
			ErrMsg: groupErrMsg,
		}, h.currentEmail(r)).Render(r.Context(), w)
}

func groupThresholdFormState(r *http.Request) templates.FormState {
	form := hostThresholdsFormState(r)
	form["scope"] = r.FormValue("scope")
	form["label_env"] = r.FormValue("label_env")
	form["label_role"] = r.FormValue("label_role")
	return form
}

// Пустые scope/label — заведомо «нет», не совпадение с пустой меткой.
func groupThresholdExists(groups []host.GroupThreshold, scope, label string) bool {
	if scope == "" || label == "" {
		return false
	}
	for _, g := range groups {
		if g.Scope == scope && g.Label == label {
			return true
		}
	}
	return false
}

func (h *Handler) hostSettingsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Metrics == nil || h.HostSettings == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderHostSettings(w, r, http.StatusOK, projectID, nil, "", nil, "")
}

func (h *Handler) hostSettingsSave(w http.ResponseWriter, r *http.Request) {
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
	// Hosts/HostOverrides обязательны здесь: без них resolveDisabledKindIncidents не может
	// посчитать каскад и не имеет права тихо откатиться к закрытию по всему проекту.
	if h.Metrics == nil || h.HostSettings == nil || h.Hosts == nil || h.HostOverrides == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	settings, err := parseHostSettingsForm(r)
	if err != nil {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, hostSettingsFormState(r), hostSettingsErrorMessage(r.Context(), err), nil, "")
		return
	}
	if err := h.HostSettings.Save(r.Context(), projectID, settings); err != nil {
		if errors.Is(err, host.ErrInvalidDiskThreshold) || errors.Is(err, host.ErrInvalidMemoryThreshold) ||
			errors.Is(err, host.ErrInvalidLoadThreshold) || errors.Is(err, host.ErrInvalidSilentAfter) {
			h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, hostSettingsFormState(r), hostSettingsErrorMessage(r.Context(), err), nil, "")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.resolveDisabledKindIncidents(r.Context(), projectID, settings)
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostSettingsPath(projectID), http.StatusSeeOther)
}

// Без потолка оператор мог завести правило со сколь угодно длинной меткой — она грузилась бы
// из БД заново на каждом тике оценщика.
const maxGroupThresholdLabelLen = 256

// Upsert идемпотентен по (project_id, scope, label): сохранение под уже существующей парой
// ЗАМЕЩАЕТ её целиком — отдельного действия «редактирование» нет.
func (h *Handler) hostGroupThresholdSave(w http.ResponseWriter, r *http.Request) {
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
	if h.Metrics == nil || h.HostSettings == nil || h.Hosts == nil || h.GroupThresholds == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	scope := r.FormValue("scope")
	label := r.FormValue("label_env")
	if scope == "role" {
		label = r.FormValue("label_role")
	}
	// scope/label не проверяются на членство в FacetValues — метка могла исчезнуть между
	// отрисовкой формы и отправкой; орфан-правило безвредно, резолвер просто не найдёт хостов.
	if (scope != "env" && scope != "role") || label == "" || utf8.RuneCountInString(label) > maxGroupThresholdLabelLen {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, nil, "",
			groupThresholdFormState(r), i18n.T(r.Context(), "error.hostsettings.group_scope_label"))
		return
	}
	ov, err := parseHostThresholdsForm(r)
	if err != nil {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, nil, "",
			groupThresholdFormState(r), hostSettingsErrorMessage(r.Context(), err))
		return
	}
	if err := h.GroupThresholds.Upsert(r.Context(), projectID, scope, label, ov); err != nil {
		if errors.Is(err, host.ErrInvalidDiskThreshold) || errors.Is(err, host.ErrInvalidMemoryThreshold) ||
			errors.Is(err, host.ErrInvalidLoadThreshold) || errors.Is(err, host.ErrInvalidSilentAfter) {
			h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, nil, "",
				groupThresholdFormState(r), hostSettingsErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostSettingsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) hostGroupThresholdDelete(w http.ResponseWriter, r *http.Request) {
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
	if h.Metrics == nil || h.HostSettings == nil || h.GroupThresholds == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	scope := r.FormValue("scope")
	label := r.FormValue("label")
	if (scope != "env" && scope != "role") || label == "" {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, nil,
			i18n.T(r.Context(), "error.hostsettings.group_scope_label"), nil, "")
		return
	}
	// CSP (default-src 'self', без unsafe-inline) не исполняет inline confirm() — поэтому
	// подтверждение отдельной страницей, а не JS-диалогом.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.host_group_threshold_delete.message", "confirm.delete",
			hostSettingsPath(projectID), hostSettingsPath(projectID)+"/groups/delete",
			[]templates.HiddenField{{Name: "scope", Value: scope}, {Name: "label", Value: label}},
			"scope", i18n.T(r.Context(), "host.threshold.scope."+scope), "label", label)
		return
	}
	if err := h.GroupThresholds.Delete(r.Context(), projectID, scope, label); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, hostSettingsPath(projectID), http.StatusSeeOther)
}

// Страница обхода хостов при закрытии — не MaxHostsPerProject: обход не должен зависеть
// от этого потолка как от границы выборки.
const resolveDisabledKindPageSize = 500

// Запас над MaxHostsPerProject/resolveDisabledKindPageSize — страховка от бесконечного
// цикла при поломке курсора, не ожидаемая длина обхода.
const maxResolveDisabledKindPages = 100

// Закрывает по каскаду ThresholdResolver.Effective, одним UPDATE на вид —
// не на каждую пару «хост × вид».
func (h *Handler) resolveDisabledKindIncidents(ctx context.Context, projectID int64, settings host.Settings) {
	// nil-safe: в отличие от Hosts/HostOverrides, обязательных по гейту hostSettingsSave.
	if h.HostIncidents == nil {
		return
	}
	var disabledKinds []string
	for _, kind := range host.Kinds {
		enabled, ok := settings.KindEnabled(kind)
		if ok && !enabled {
			disabledKinds = append(disabledKinds, kind)
		}
	}
	if len(disabledKinds) == 0 {
		return
	}

	var groups []host.GroupThreshold
	if h.GroupThresholds != nil {
		var err error
		groups, err = h.GroupThresholds.List(ctx, projectID)
		if err != nil {
			slog.Error("web: resolve incidents of disabled host threshold: groups",
				"project_id", projectID, "error", err)
			return
		}
	}

	toClose := make(map[string][]int64, len(disabledKinds))
	after := ""
	for page := 0; ; page++ {
		if page >= maxResolveDisabledKindPages {
			slog.Error("web: resolve incidents of disabled host threshold: page limit exceeded",
				"project_id", projectID, "pages", page)
			return
		}
		hosts, err := h.Hosts.ListPage(ctx, projectID, after, resolveDisabledKindPageSize)
		if err != nil {
			slog.Error("web: resolve incidents of disabled host threshold: list hosts",
				"project_id", projectID, "error", err)
			return
		}
		if len(hosts) == 0 {
			break
		}
		ids := make([]int64, len(hosts))
		for i, hst := range hosts {
			ids[i] = hst.ID
		}
		overrides, err := h.HostOverrides.GetForHosts(ctx, ids)
		if err != nil {
			slog.Error("web: resolve incidents of disabled host threshold: overrides",
				"project_id", projectID, "error", err)
			return
		}
		resolver := host.ThresholdResolver{
			Project:       settings,
			ProjectExists: true,
			Groups:        groups,
			Overrides:     overrides,
		}
		for _, hst := range hosts {
			eff := resolver.Effective(hst)
			for _, kind := range disabledKinds {
				if enabled, _ := eff.Settings.KindEnabled(kind); !enabled {
					toClose[kind] = append(toClose[kind], hst.ID)
				}
			}
		}
		if len(hosts) < resolveDisabledKindPageSize {
			break
		}
		after = hosts[len(hosts)-1].Name
	}

	for kind, ids := range toClose {
		n, err := h.HostIncidents.ResolveOpenByHostsKind(ctx, ids, kind)
		if err != nil {
			slog.Error("web: resolve incidents of disabled host threshold",
				"project_id", projectID, "kind", kind, "error", err)
			continue
		}
		if n > 0 {
			slog.Info("web: host threshold disabled, open incidents resolved",
				"project_id", projectID, "kind", kind, "resolved", n)
		}
	}
}

// CanAccessProject — гейт всей страницы; CanOperate для порогов/удаления проверяется отдельно
// и не блокирует просмотр карточки не-оператору.
func (h *Handler) hostDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Metrics == nil || h.Hosts == nil || h.HostIncidents == nil || h.HostSettings == nil || h.HostOverrides == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	name := r.PathValue("name")
	hst, found, err := h.Hosts.Get(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}
	h.renderHostDetail(w, r, http.StatusOK, projectID, uid, hst, nil, "")
}

// eff — эффективные пороги хоста по каскаду host-override → role-group → env-group →
// project → default (ThresholdResolver.Effective).
func (h *Handler) renderHostDetail(w http.ResponseWriter, r *http.Request, status int, projectID, uid int64, hst host.Host, thresholdForm templates.FormState, thresholdErr string) {
	name := hst.Name

	projSettings, projExists, err := h.HostSettings.GetWithExists(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// nil-safe: пустой список групп резолвер трактует как «групповых порогов нет».
	var groups []host.GroupThreshold
	if h.GroupThresholds != nil {
		groups, err = h.GroupThresholds.List(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	hostOverride, err := h.HostOverrides.Get(r.Context(), hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	eff := host.ThresholdResolver{
		Project:       projSettings,
		ProjectExists: projExists,
		Groups:        groups,
		Overrides:     map[int64]host.ThresholdOverride{hst.ID: hostOverride},
	}.Effective(hst)

	tr := h.resolveTimeRange(w, r, "24h")
	from, to := tr.From, tr.To
	step := autoStep(tr.Window(), time.Minute, 0, metricChartBuckets)

	openIncidents, err := h.HostIncidents.ListOpenByHost(r.Context(), hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	recentIncidents, err := h.hostRecentIncidents(r.Context(), hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	openKinds := make([]string, 0, len(openIncidents))
	for _, inc := range openIncidents {
		openKinds = append(openKinds, inc.Kind)
	}
	statusKind, problemKinds := hostRowStatus(openKinds, hst.LastSeen, time.Now(), eff.Settings)

	// Отказ ClickHouse не роняет карточку: графики скрываются, остальное остаётся.
	charts, err := h.hostDetailCharts(r.Context(), projectID, name, from, to, step, eff.Settings)
	chartsFailed := err != nil
	if chartsFailed {
		slog.Warn("web: host charts failed", "project_id", projectID, "host", name, "error", err)
		charts = nil
	}

	// Окно — hostsListWindow, не выбранный на странице период: нужно самое свежее значение
	// «сейчас», а не история за vm.Range.
	var uptimeStr string
	now := time.Now()
	uptimeByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, hostmetric.Uptime,
		nil, "", "max", now.Add(-hostsListWindow), now)
	if err != nil {
		slog.Warn("web: host uptime query failed", "project_id", projectID, "host", name, "error", err)
	} else if sec, ok := uptimeByHost[name]; ok {
		uptimeStr = humanize.Duration(r.Context(), time.Duration(sec*float64(time.Second)))
	}

	// Не requireProjectOperator: тот рендерит 404 всей странице, а карточку должен видеть
	// любой с доступом к проекту.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Пусто, если раздача бинарей не сконфигурирована или BaseURL небезопасен — тогда бейдж
	// "есть обновление" остаётся честным, а готовую команду карточка не предлагает.
	agentUpdateCmd := ""
	if h.agentDistAvailable() && agentBaseURLSecure(h.BaseURL) {
		agentUpdateCmd = agentUpdateCommand(h.BaseURL)
	}

	serverVersion := version.Version()

	ackedByIDs := make([]int64, 0, len(openIncidents))
	for _, inc := range openIncidents {
		if inc.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *inc.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	vm := templates.HostDetailVM{
		ProjectID:            projectID,
		Host:                 hst,
		Range:                timeRangeVM(tr),
		StatusKind:           statusKind,
		ProblemKinds:         problemKinds,
		OpenIncidents:        openIncidents,
		RecentIncidents:      recentIncidents,
		AckedBy:              ackedBy,
		Charts:               charts,
		ChartsFailed:         chartsFailed,
		CanOperate:           canOperate,
		Uptime:               uptimeStr,
		AgentVersion:         hst.AgentVersion,
		AgentUpdateAvailable: agentUpdateAvailable(hst.AgentVersion, serverVersion),
		AgentUpdateCmd:       agentUpdateCmd,
		ServerVersion:        serverVersion,
		IsNew:                now.Sub(hst.FirstSeen) < hostNewWindow,
		Override:             hostOverride,
		Effective:            eff,
		ThresholdsForm:       thresholdForm,
		ThresholdsErr:        thresholdErr,
	}
	w.WriteHeader(status)
	_ = templates.HostDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) hostThresholdsSave(w http.ResponseWriter, r *http.Request) {
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
	if h.Metrics == nil || h.Hosts == nil || h.HostIncidents == nil || h.HostSettings == nil || h.HostOverrides == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	name := r.PathValue("name")
	hst, found, err := h.Hosts.Get(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	ov, err := parseHostThresholdsForm(r)
	if err != nil {
		h.renderHostDetail(w, r, http.StatusUnprocessableEntity, projectID, uid, hst, hostThresholdsFormState(r), hostSettingsErrorMessage(r.Context(), err))
		return
	}
	// Save зовёт ValidateOverride сам — отдельного вызова здесь не нужно.
	if err := h.HostOverrides.Save(r.Context(), hst.ID, ov); err != nil {
		if errors.Is(err, host.ErrInvalidDiskThreshold) || errors.Is(err, host.ErrInvalidMemoryThreshold) ||
			errors.Is(err, host.ErrInvalidLoadThreshold) || errors.Is(err, host.ErrInvalidSilentAfter) {
			h.renderHostDetail(w, r, http.StatusUnprocessableEntity, projectID, uid, hst, hostThresholdsFormState(r), hostSettingsErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostDetailPath(projectID, name), http.StatusSeeOther)
}

const hostRecentIncidentsLimit = 20

func (h *Handler) hostRecentIncidents(ctx context.Context, hostID int64) ([]host.Incident, error) {
	return h.HostIncidents.ListRecentByHost(ctx, hostID, hostRecentIncidentsLimit)
}

func (h *Handler) hostDelete(w http.ResponseWriter, r *http.Request) {
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
	if h.Metrics == nil || h.Hosts == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	name := r.PathValue("name")
	if !h.parseForm(w, r) {
		return
	}
	// CSP не исполняет inline confirm() — подтверждение отдельной страницей, не JS-диалогом.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.host_delete.message", "confirm.delete",
			hostDetailPath(projectID, name), hostDeletePath(projectID, name), nil,
			"name", name)
		return
	}
	deleted, err := h.Hosts.Delete(r.Context(), projectID, name)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if deleted && h.HostForget != nil {
		h.HostForget.Forget(projectID, name)
	}
	// Только при фактическом удалении: deleted=false — гонка вкладок или повтор POST, и
	// «Удалено» тогда сообщало бы о том, чего не произошло.
	if deleted {
		h.flashOK(w, "flash.deleted", 0)
	}
	http.Redirect(w, r, hostsPath(projectID), http.StatusSeeOther)
}
