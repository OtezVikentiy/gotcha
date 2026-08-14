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
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// HostForgetter — инвалидация троттлера регистрации хостов при удалении
// (см. host.Toucher.Forget). Интерфейс, а не конкретный *host.Toucher —
// Handler.HostForget остаётся nil-safe в стендах/режимах без ingest.
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

// hostsListWindow — окно «последних значений» метрик хоста в списке (спека
// §5.2): свежее, чем период инцидентов, потому что список — снимок «сейчас»,
// а не история.
const hostsListWindow = 15 * time.Minute

// hostsListLimit — потолок числа строк на странице списка (без пагинации —
// §5.2); превышение помечается i18n-подсказкой в шаблоне.
const hostsListLimit = 500

// hostsList — GET /projects/{id}/hosts: список хостов проекта со сводным
// статусом (ok / открытые инциденты / тихий) и последними значениями CPU/
// RAM/диск/load за 15 минут. Гейт — как у метрик (h.Metrics == nil → 404),
// плюс отдельная проверка Hosts/HostIncidents/HostSettings: main.go
// действительно проставляет их всегда вместе с Metrics, но тестовые стенды
// (shell_operate_e2e_test.go, authz_behavior_test.go) этот инвариант уже
// нарушают, вооружая только Metrics — без своего гейта здесь была бы паника
// на nil-указателе, а не честный 404.
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

	// hostsListLimit+1 — ровно столько, чтобы отличить «влезло» от «есть ещё»
	// (см. truncated ниже) и не вычитывать ради подсказки весь реестр проекта.
	hosts, err := h.Hosts.List(r.Context(), projectID, hostsListLimit+1)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	settings, err := h.HostSettings.Get(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Открытые инциденты, свёрнутые по host_id → виды (disk/memory/load/
	// silent). ListOpenByProject, а не ListByProject с лимитом: последний
	// отдаёт последние N инцидентов ЛЮБОГО статуса, и в проекте, где закрытых
	// накопилось больше лимита, открытый инцидент не попадал в выборку вовсе —
	// хост с живой проблемой показывался спокойным (ревью I3). Открытых по
	// построению немного (не больше одного на пару host_id+kind), лимит им не
	// нужен.
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

	// Двухуровневая агрегация (§5.2, B1): CPU busy% = 1 − idle-доля усреднённая
	// по ядрам (subKey="cpu", subAgg=avg); худший диск = max по mountpoint'ам;
	// load/core делится в Go, а не в SQL — обе метрики читаются отдельно.
	idleByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, "system.cpu.utilization",
		[]metric.LabelMatcher{{Key: "state", Value: "idle"}}, "cpu", "avg", from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	memByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, "system.memory.utilization",
		[]metric.LabelMatcher{{Key: "state", Value: "used"}}, "", "", from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	diskByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, "system.filesystem.utilization",
		nil, "mountpoint", "max", from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	load5mByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, "system.cpu.load_average.5m",
		nil, "", "", from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	coresByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, "system.cpu.logical.count",
		nil, "", "", from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	truncated := len(hosts) > hostsListLimit
	if truncated {
		hosts = hosts[:hostsListLimit]
	}
	rows := make([]templates.HostRowVM, 0, len(hosts))
	for _, hst := range hosts {
		row := templates.HostRowVM{Name: hst.Name, LastSeen: hst.LastSeen}
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
		row.StatusKind, row.OpenKinds = hostRowStatus(openKindsByHost[hst.ID], hst.LastSeen, now, settings)
		rows = append(rows, row)
	}
	sortHostRows(rows)

	// Конфиг коллектора нужен обеим веткам страницы (UX-аудит A1, P2-11):
	// онбордингу пустого списка (§5.4) и свёрнутому блоку под непустым —
	// второй сервер подключают тем же YAML, а страница порогов, где он лежал
	// ещё раз, закрыта гейтом оператора. Цена — один запрос ключей проекта на
	// отрисовку списка; граница видимости та же, что у DSN (CanAccessProject
	// проверен выше).
	config := h.hostCollectorConfig(r.Context(), projectID)

	_ = templates.HostsList(projectID, rows, truncated, hostsListLimit, config, h.currentEmail(r)).Render(r.Context(), w)
}

// hostRowStatus классифицирует статус строки хоста по открытым инцидентам
// (openKinds — виды host_incidents, открытые именно на ЭТОМ хосте) и
// last_seen: "problem" (есть открытый инцидент вида, отличного от
// "silent" — вид "silent" исключён нарочно, см. ниже), "silent" (тишина —
// по last_seen ИЛИ по открытому incident kind="silent") или "ok".
//
// kind="silent" исключён из проблемных сознательно (находка ревью T14):
// host.Evaluator (план A1) открывает host_incidents с kind="silent" по
// тому же порогу SilentAfter, но реже (тик раз в HostEvalInterval, обычно
// 60с) — до правки хост в первые ~60с тишины показывал бейдж "Тихий" (тир
// silent, посчитанный здесь по last_seen), а после первого тика evaluator'а,
// никак не изменившись по сути, переезжал в тир "problem" ("Тишина",
// badge-danger), потому что silent тоже присутствовал среди OpenKinds.
// Тишина — всегда ОДИН тир независимо от того, кто её первым заметил.
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

// hostRowRank — порядок сортировки статус-бейджей: проблемные хосты сверху,
// затем тихие, затем спокойные (§5.2 «проблемные сверху, затем по имени»,
// расширено тихими как промежуточным уровнем важности).
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

// sortHostRows сортирует строки списка: проблемные → тихие → ok, внутри
// каждой группы по имени (SliceStable — стабильный порядок при повторном
// рендере с теми же данными).
func sortHostRows(rows []templates.HostRowVM) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := hostRowRank(rows[i].StatusKind), hostRowRank(rows[j].StatusKind)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Name < rows[j].Name
	})
}

// collectorConfigTmpl — готовый конфиг otelcol-contrib для сбора хостовых
// метрик (§5.4 дизайна). Собирается через fmt.Sprintf строкой, а не YAML-
// маршалингом структуры: это конфиг ЧУЖОЙ программы (otelcol-contrib), не
// внутренняя структура продукта — маршалинг завёл бы Go-типы под формат,
// которым мы не управляем и не тестируем сами, ради двух подстановок.
//
// system.cpu.logical.count включена явно (делитель load/core, §4.1) —
// избыточно, если в конкретной версии otelcol-contrib она и так входит в
// default-набор, но безвредно, а страхует от версий, где это не так
// (metadata.yaml default-набор зависит от версии, m10 ревью плана). endpoint
// — БАЗОВЫЙ URL без /v1/metrics: путь дописывает сам otlphttp-экспортёр.
//
// exclude_fs_types/exclude_mount_points у скрейпера filesystem — не
// косметика, а условие работоспособности порога диска (ревью I1). Скрейпер
// без исключений отдаёт ВСЕ смонтированные ФС, а встроенный порог берёт по
// хосту максимум по mountpoint'ам: на обычной Ubuntu каждый установленный
// snap смонтирован squashfs'ом, заполненным на 100% ПО ЗАМЫСЛУ (образ ровно
// по размеру содержимого) — дефолтный порог «>90%» открывал бы инцидент на
// первом же тике оценщика, закрыть который нечем, потому что диск на самом
// деле свободен. Тот же класс мусора — tmpfs/devtmpfs (ОЗУ), overlay
// (контейнерные слои поверх уже посчитанного корня) и псевдо-ФС ядра; в
// топ-8 графика занятости они вытесняли реальные разделы. Синтаксис
// (fs_types/mount_points + match_type: strict|regexp) — из README
// hostmetricsreceiver; регулярные выражения якорим на ^, потому что
// filterset/regexp матчит подстроку.
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
          fs_types: [autofs, binfmt_misc, bpf, cgroup, cgroup2, configfs, debugfs,
            devpts, devtmpfs, efivarfs, fusectl, hugetlbfs, iso9660, mqueue, nsfs,
            overlay, proc, pstore, ramfs, securityfs, squashfs, sysfs, tmpfs, tracefs]
        exclude_mount_points:
          match_type: regexp
          mount_points: [^/snap/.*, ^/var/lib/docker/.*, ^/var/lib/kubelet/.*,
            ^/run/.*, ^/dev/.*, ^/proc/.*, ^/sys/.*]
        metrics:
          system.filesystem.utilization: {enabled: true}
      disk: {}
      network: {}
      load: {}
      processes: {}
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

// collectorConfig собирает готовый YAML коллектора: baseURL — BaseURL
// инстанса (без пути), key — публичный ключ проекта, идущий в заголовок
// Authorization как DSN-эквивалент для хостовых метрик.
func collectorConfig(baseURL, key string) string {
	return fmt.Sprintf(collectorConfigTmpl, baseURL, key)
}

// hostCollectorConfig собирает конфиг коллектора для проекта: BaseURL +
// первый активный публичный ключ (та же граница видимости, что у DSN на
// странице настройки проекта — buildDSN/firstLiveKey, onboarding.go).
// Пустая строка — в проекте нет ни одного активного ключа (шаблон решает,
// что показать вместо конфига) или чтение ключей провалилось (запасной путь
// — не заваливать всю страницу списка/настроек ради вспомогательного блока).
func (h *Handler) hostCollectorConfig(ctx context.Context, projectID int64) string {
	keys, err := h.Org.KeysForProject(ctx, projectID)
	if err != nil {
		return ""
	}
	key := firstLiveKey(keys)
	if key == "" {
		return ""
	}
	return collectorConfig(h.BaseURL, key)
}

// hostSettingsFormState — введённые значения формы порогов для повторной
// отрисовки после ошибки валидации (см. FormState, metricRuleFormState).
// Чекбоксы кодируются явно "1"/"0", а не отсутствием ключа: HTML не шлёт
// снятый чекбокс вовсе, и пустая карта была бы неотличима от первого
// открытия формы (GET, form == nil), где значения берутся из настроек, а не
// из (никогда не отправлявшейся) формы.
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

// invalidThresholdFloat — то же условие, что у metricAlertCreate
// (metricalerts.go:121-122, бриф прямо называл этот файл образцом):
// strconv.ParseFloat принимает "NaN"/"Inf"/"+Inf" БЕЗ ошибки, а
// host.Validate сравнивает результат с границами через <=/> — сравнение с
// NaN всегда false в обе стороны ("NaN <= 0" и "NaN > 1" одновременно
// ложны), поэтому такой порог тихо проходит Validate и попадает в БД
// (Postgres double precision и CHECK (load_threshold > 0) тоже принимают
// NaN/Infinity). Дальше оценщик host.Evaluator никогда не срабатывает
// ("значение > NaN"/"значение > Inf" всегда false) — тихая порча настроек:
// пользователь уверен, что порог включён и защищает, а по факту он мёртв.
func invalidThresholdFloat(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0)
}

// parseHostSettingsForm разбирает форму порогов в host.Settings. Проценты
// диска/памяти конвертируются в доли на этой границе (§4.2 дизайна — «в UI
// диск/RAM — проценты, храним долями»), минуты тишины — в time.Duration.
// Ошибка разбора числа (нечисловой ввод ИЛИ NaN/Inf, см. invalidThresholdFloat)
// возвращается тем же сентинелом host.ErrInvalid*, что и Validate у выхода за
// границу значения — с точки зрения пользователя это один класс ошибки «поле
// заполнено не так», и hostSettingsErrorMessage сопоставляет оба случая
// одним и тем же текстом.
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
	// Границу проверяем ДО перевода в Duration: 10^12 минут переполняют int64
	// наносекунд молча, и в host.Validate приехало бы отрицательное значение —
	// формально «меньше минимума», фактически 500-я на переполнении int4 в
	// колонке, если знак совпадёт. Отрицательные и нулевые отсекаются здесь же.
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

// hostSettingsErrorMessage переводит сентинел-ошибки host.Validate (и
// parseHostSettingsForm) в понятное сообщение — тот же приём, что
// maintenanceErrorMessage: errors.Is по каждому сентинелу вместо показа
// err.Error() (Go-текста "host: disk threshold must be in (0, 1]: got 1.5")
// пользователю.
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

// renderHostSettings отрисовывает страницу настроек: текущие пороги
// (h.HostSettings.Get — DefaultSettings, если строка ещё не сохранялась) +
// конфиг коллектора под свёрнутой ссылкой. form/errMsg — введённые
// пользователем значения при ошибке валидации (см. FormState).
func (h *Handler) renderHostSettings(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	settings, err := h.HostSettings.Get(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	config := h.hostCollectorConfig(r.Context(), projectID)
	w.WriteHeader(status)
	_ = templates.HostSettings(projectID, settings, config, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// hostSettingsPage — GET /projects/{id}/hosts/settings: форма порогов
// встроенных инцидентов хоста (§5.5 дизайна). Гейт — оператор проекта
// (requireProjectOperator), как в §5.1.
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
	h.renderHostSettings(w, r, http.StatusOK, projectID, nil, "")
}

// hostSettingsSave — POST /projects/{id}/hosts/settings: сохранить пороги.
// Гейт — оператор + sameOrigin. Ошибка валидации (значение вне границы или
// нечисловой ввод) → 422 с переотрисовкой формы и введёнными значениями
// (FormState) — тот же приём, что у metricAlertCreate/maintenanceCreate.
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
	if h.Metrics == nil || h.HostSettings == nil {
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
	settings, err := parseHostSettingsForm(r)
	if err != nil {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, hostSettingsFormState(r), hostSettingsErrorMessage(r.Context(), err))
		return
	}
	if err := h.HostSettings.Save(r.Context(), projectID, settings); err != nil {
		if errors.Is(err, host.ErrInvalidDiskThreshold) || errors.Is(err, host.ErrInvalidMemoryThreshold) ||
			errors.Is(err, host.ErrInvalidLoadThreshold) || errors.Is(err, host.ErrInvalidSilentAfter) {
			h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, hostSettingsFormState(r), hostSettingsErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.resolveDisabledKindIncidents(r.Context(), projectID, settings)
	// Форма порогов возвращается на саму себя с теми же значениями, что были
	// введены, — без flash сохранение выглядело как «ничего не произошло»
	// (UX-аудит A1, P1-5). Тот же приём, что у maintenance.go и alerts.go.
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostSettingsPath(projectID), http.StatusSeeOther)
}

// resolveDisabledKindIncidents закрывает открытые инциденты видов, которые
// только что стали выключенными.
//
// Без этого выключение порога не имеет обратной силы: Evaluator.Tick
// выключенный вид пропускает целиком, закрыть его инцидент некому, а ручного
// закрытия инцидента хоста в интерфейсе нет — оператор, выключивший шумный
// порог, оставался бы с вечно красным бейджем на списке хостов (ревью I2).
//
// Вызывается ПОСЛЕ успешного Save и ошибку наружу не отдаёт: настройки уже
// сохранены, и 500 после этого солгал бы («не сохранилось»), а повторный POST
// той же формы — обычный способ повторить попытку. Незакрытый инцидент виден
// в логе и переживёт только до следующего сохранения.
func (h *Handler) resolveDisabledKindIncidents(ctx context.Context, projectID int64, settings host.Settings) {
	// В стендах без ingest HostIncidents может быть не проведён (гейт
	// hostSettingsSave проверяет только Metrics/HostSettings) — nil-safe, как
	// HostForget у hostDelete.
	if h.HostIncidents == nil {
		return
	}
	for _, kind := range host.Kinds {
		enabled, ok := settings.KindEnabled(kind)
		if !ok || enabled {
			continue
		}
		n, err := h.HostIncidents.ResolveOpenByProjectKind(ctx, projectID, kind)
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

// hostDetail — GET /projects/{id}/hosts/{name}: карточка хоста (§5.3
// дизайна) — семь графиков (CPU/RAM/диск-занятость/диск-IO/сеть/load/
// процессы), открытые инциденты + недавняя история, кнопка удаления (только
// оператору). Гейт — CanAccessProject, как у списка: карточку смотрит любой
// с доступом к проекту, действие удаления требует оператора отдельно (ниже,
// на кнопке — CanOperate вычисляется здесь read-only, без гейта всей
// страницы). Несуществующее имя хоста → 404 (существование — часть
// проверки, чтобы страница не отдавала 200 на произвольный путь).
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

	settings, err := h.HostSettings.Get(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	tr := h.resolveTimeRange(w, r, "24h")
	from, to := tr.From, tr.To
	step := autoStep(tr.Window(), time.Minute, 0, metricChartBuckets)

	openIncidents, err := h.HostIncidents.ListOpenByHost(r.Context(), hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	recentIncidents, err := h.hostRecentIncidents(r.Context(), projectID, hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	openKinds := make([]string, 0, len(openIncidents))
	for _, inc := range openIncidents {
		openKinds = append(openKinds, inc.Kind)
	}
	statusKind, problemKinds := hostRowStatus(openKinds, hst.LastSeen, time.Now(), settings)

	charts, err := h.hostDetailCharts(r.Context(), projectID, name, from, to, step, settings)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// CanOperate — read-only: считаем прямо здесь (не через requireProjectOperator,
	// который на отказе рендерит 404 всей странице) ровно как canManage у
	// MonitorDetail — просмотр карточки доступен всем с доступом к проекту,
	// кнопка удаления просто скрыта у не-оператора.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	vm := templates.HostDetailVM{
		ProjectID:       projectID,
		Host:            hst,
		Range:           timeRangeVM(tr),
		StatusKind:      statusKind,
		ProblemKinds:    problemKinds,
		OpenIncidents:   openIncidents,
		RecentIncidents: recentIncidents,
		Charts:          charts,
		CanOperate:      canOperate,
	}
	_ = templates.HostDetail(vm, h.currentEmail(r)).Render(r.Context(), w)
}

// hostRecentIncidentsScan — сколько последних инцидентов ПРОЕКТА просматривать
// в поисках инцидентов ЭТОГО хоста. IncidentService не даёт метода «по хосту,
// любой статус» (ListOpenByHost — только открытые) — заводить его ради одной
// карточки не стоит: инцидентов на проект немного, лимит просмотра щедрый.
//
// Лимит здесь безопасен, в отличие от списка хостов (ревью I3): статус и
// открытые инциденты карточка берёт ListOpenByHost'ом — без лимита и без
// примеси чужих хостов, — а этот блок показывает только НЕДАВНЮЮ историю. Его
// худший случай — «истории не видно», а не «проблема не показана».
const hostRecentIncidentsScan = 500

// hostRecentIncidentsLimit — потолок «последних инцидентов» блока карточки
// (§5.3, бриф T15): открытые показываются отдельно и полностью
// (ListOpenByHost), здесь — недавняя история независимо от статуса.
const hostRecentIncidentsLimit = 20

// hostRecentIncidents фильтрует ListByProject (уже отсортирован по
// started_at DESC) до инцидентов ЭТОГО хоста, capped на hostRecentIncidentsLimit.
func (h *Handler) hostRecentIncidents(ctx context.Context, projectID, hostID int64) ([]host.Incident, error) {
	all, err := h.HostIncidents.ListByProject(ctx, projectID, hostRecentIncidentsScan)
	if err != nil {
		return nil, err
	}
	out := make([]host.Incident, 0, hostRecentIncidentsLimit)
	for _, inc := range all {
		if inc.HostID != hostID {
			continue
		}
		out = append(out, inc)
		if len(out) >= hostRecentIncidentsLimit {
			break
		}
	}
	return out, nil
}

// hostDelete — POST /projects/{id}/hosts/{name}/delete: удалить хост
// (двухшаговое подтверждение, host.Store.Delete, h.HostForget.Forget —
// nil-safe — для немедленной реактивации троттлера регистрации, см. §2.4
// дизайна). Гейт — оператор + sameOrigin, как у остальных мутаций мониторинга.
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия. Имя
	// хоста уже часть action-URL (тот же маршрут), отдельного hidden-поля не
	// нужно — второй POST придёт на тот же path.
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
	// Flash только на фактическом удалении: deleted=false — хост уже исчез
	// (гонка двух вкладок либо повтор второго POST по «назад»), и «Удалено»
	// в этом случае сообщало бы о действии, которого этот запрос не совершал.
	if deleted {
		h.flashOK(w, "flash.deleted", 0)
	}
	http.Redirect(w, r, hostsPath(projectID), http.StatusSeeOther)
}
