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

// hostNewWindow — окно «новый хост» (B1, T5): бейдж row.IsNew и SQL-ветка
// HostFilter.NewOnly (host.Store.ListFiltered, `interval '24 hours'`)
// обязаны использовать ОДНО и то же число — иначе фильтр по чипу «новые» и
// бейдж «новый» у той же строки молча разъехались бы. Определён ЗДЕСЬ (не в
// internal/host — фильтр там оперирует SQL-литералом, а не значением этой
// константы) единственный раз; всё, что рисует бейдж/чип «новый» (в том
// числе T7), только ПОТРЕБЛЯЕТ hostNewWindow, не переопределяет его заново.
const hostNewWindow = 24 * time.Hour

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

	// Фильтр читается из query СТРОГО на стороне SQL (host.Store.ListFiltered,
	// WHERE), не Go-срезом поверх List — план-ревью Major-2: при парке,
	// упёршемся в hostsListLimit+1, срез поверх уже усечённой выборки давал бы
	// ложную пустоту вместо настоящих совпадений за пределами страницы.
	q := r.URL.Query()
	filter := host.HostFilter{
		Environment: q.Get("env"),
		Role:        q.Get("role"),
		NewOnly:     q.Get("new") == "1",
	}
	// group — только вью-слой (T6): режет уже отфильтрованные/отсортированные
	// rows на секции, не участвует в SQL WHERE (host.HostFilter выше).
	group := normalizeHostGroup(q.Get("group"))

	// hostsListLimit+1 — ровно столько, чтобы отличить «влезло» от «есть ещё»
	// (см. truncated ниже) и не вычитывать ради подсказки весь реестр проекта.
	hosts, err := h.Hosts.ListFiltered(r.Context(), projectID, filter, hostsListLimit+1)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Значения фасетов — ВСЕГДА по полному реестру проекта (без учёта текущего
	// фильтра), не по уже отфильтрованной hosts выше: иначе выбор одного
	// значения env стирал бы из сайдбара все прочие значения env, которые
	// перестали совпадать сами с собой — фасет должен предлагать переключение
	// на любое другое значение, а не только те, что видны в текущей выборке.
	envValues, roleValues, err := h.Hosts.FacetValues(r.Context(), projectID)
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
	// Колонки метрик — из ClickHouse; сам список хостов — из PostgreSQL.
	// Отказ CH не роняет страницу (единый приём CH-страниц, образец —
	// logsList): хосты и их статусы показываем, колонки метрик пустые, над
	// таблицей — «метрики временно недоступны». Первый же отказ прекращает
	// опрос: остальные четыре запроса к тому же хранилищу лишь потянули бы
	// время ответа.
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
		row.StatusKind, row.OpenKinds = hostRowStatus(openKindsByHost[hst.ID], hst.LastSeen, now, settings)
		rows = append(rows, row)
	}
	sortHostRows(rows)

	// Команда агента и конфиг коллектора нужны обеим веткам страницы
	// (UX-аудит A1, P2-11): онбордингу пустого списка (§5.4) и свёрнутому
	// блоку под непустым — второй сервер подключают тем же путём, а страница
	// порогов, где эти блоки лежали ещё раз, закрыта гейтом оператора. Цена —
	// один запрос ключей проекта на отрисовку списка (hostInstallBlocks);
	// граница видимости та же, что у DSN (CanAccessProject проверен выше).
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

// normalizeHostGroup приводит query-параметр group к одному из {"", "env",
// "role"} (T6): "" — значение по умолчанию (без группировки) — hostsFilterURL
// (web/templates/hosts.templ) никогда не пишет его в query, тем же принципом,
// что new=0 никогда не появляется в ссылках чипов. Любое незнакомое значение
// (опечатка в URL, старая закладка) — тоже "", а не 500.
func normalizeHostGroup(v string) string {
	if v == "env" || v == "role" {
		return v
	}
	return ""
}

// groupHostRows делит уже отфильтрованные и отсортированные (sortHostRows)
// rows на секции по значению Environment/Role (T6): пустое значение метки —
// отдельная секция с i18n-подписью hosts.label.none (тем же сентинелом, что
// и у фасетов, NewHostsFacets). Секции сортируются по итоговому
// (локализованному) label; порядок строк ВНУТРИ секции не меняется —
// группировка режет уже готовый порядок sortHostRows, не пересортировывает.
// group вне {"env","role"} (в т.ч. "") — nil: HostsList в этом случае рисует
// прежнюю плоскую таблицу над rows напрямую.
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
// которым мы не управляем и не тестируем сами, ради четырёх подстановок.
//
// system.cpu.logical.count включена явно (делитель load/core, §4.1) —
// избыточно, если в конкретной версии otelcol-contrib она и так входит в
// default-набор, но безвредно, а страхует от версий, где это не так
// (metadata.yaml default-набор зависит от версии, m10 ревью плана). endpoint
// — БАЗОВЫЙ URL без /v1/metrics: путь дописывает сам otlphttp-экспортёр.
//
// system-скрейпер с system.uptime добавлен для паритета путей (§3.4
// спеки): аптайм должен ехать и с самого коллектора, не только с
// собственного Go-агента (A2) — иначе владелец, поставивший только
// otelcol-contrib без агента, останется без метрики «сколько хост живёт».
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
// filterset/regexp матчит подстроку. Оба списка (fs_types и regexp'ы точек
// монтирования) рендерятся из internal/hostmetric.ExcludedFSTypes/
// ExcludedMountPrefixes — единственного источника правды, общего с агентом
// (см. package doc hostmetric): правка исключений в одном месте меняет и
// то, что фильтрует свой агент, и то, что отдаётся владельцу в этом YAML.
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

// collectorConfig собирает готовый YAML коллектора: baseURL — BaseURL
// инстанса (без пути), key — публичный ключ проекта, идущий в заголовок
// Authorization как DSN-эквивалент для хостовых метрик. Списки исключений
// ФС генерируются из hostmetric.ExcludedFSTypes/ExcludedMountPrefixes —
// источник один с агентом (см. комментарий к collectorConfigTmpl).
func collectorConfig(baseURL, key string) string {
	fsTypes := strings.Join(hostmetric.ExcludedFSTypes, ", ")
	mountPoints := make([]string, len(hostmetric.ExcludedMountPrefixes))
	for i, p := range hostmetric.ExcludedMountPrefixes {
		mountPoints[i] = "^" + p + ".*"
	}
	return fmt.Sprintf(collectorConfigTmpl, fsTypes, strings.Join(mountPoints, ", "), baseURL, key)
}

// hostInstallBlocks собирает обе готовые строки подключения хоста — команду
// установки собственного Go-агента (путь по умолчанию, T14) и конфиг
// коллектора otelcol-contrib (свёрнутая альтернатива) — по ОДНОМУ чтению
// ключей проекта: обе строки делят один и тот же ключ типа agent
// (liveKeyFor(keys, org.KindAgent), onboarding.go). Именно agent, а не
// server: конфиг коллектора несёт процессор resourcedetection и тем самым
// РЕГИСТРИРУЕТ хост (§7 дизайна) — это единственный тип, которому разрешена
// регистрация.
//
// installCmd и config пусты одновременно, если в проекте нет ни одного
// активного ключа, или чтение ключей провалилось (запасной путь — не
// заваливать страницу списка/настроек ради вспомогательных блоков); тогда
// agentReason == "" и шаблон показывает общую подсказку hosts.onboarding.no_key
// под обоими блоками (ключом не заполнить ни один).
//
// Если ключ есть, а installCmd всё равно пуст — agentReason объясняет,
// почему путь агента недоступен, а коллектор (config) при этом остаётся
// заполненным (rem-A sec-M1/sec-M4, не показываем заведомо мёртвую или
// небезопасную команду):
//   - "dist" — раздача бинарей не сконфигурирована/каталог физически не
//     существует на этом инстансе (h.agentDistAvailable()); типично для
//     сборки не из Docker-образа. Команда установки вела бы к 404.
//   - "insecure" — BaseURL не https:// и не локальный (agentBaseURLSecure);
//     команда исполняется под root, а без TLS и SHA256SUMS, и сам бинарь
//     идут по каналу, где их подменяет MITM.
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

// isLocalBaseURL — BaseURL указывает на локальную разработку (localhost/
// loopback). Продублировано из cmd/gotcha/config.go: обе версии — короткие
// чистые функции над net/url, а cmd/gotcha — пакет main, отсюда его не
// импортировать; заводить общий пакет ради одной функции дороже дублирования.
func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// agentBaseURLSecure — BaseURL безопасен для команды установки/обновления
// агента (sec-M4): https:// (канал шифрован) или локальный адрес
// (isLocalBaseURL — та же граница, что cmd/gotcha/config.go применяет к
// прочим небезопасным дефолтам на localhost-стендах). При http:// на
// произвольном хосте команда исполняется под root, а SHA256SUMS едет тем же
// MITM-уязвимым каналом, что и сам бинарь — сверка сумм не защищает.
func agentBaseURLSecure(baseURL string) bool {
	return strings.HasPrefix(baseURL, "https://") || isLocalBaseURL(baseURL)
}

// agentInstallCommand — команда установки/регистрации собственного Go-агента
// (A2, §2.1 спеки): endpoint и ключ проекта передаются переменными
// окружения, install.sh сам ставит бинарь и systemd-юнит. Полная загрузка
// скрипта в подстановку команды перед исполнением (`sh -c "$(curl ...)"`, а
// не `curl | sh`) — по замыслу симметрична agentUpdateCommand, а не
// проверяется целостность: обе формы одинаково уязвимы MITM без TLS-пиннинга,
// разница только в том, что `$(...)` не начинает выполнять байты по мере
// получения потоковым pipe'ом.
func agentInstallCommand(baseURL, key string) string {
	return fmt.Sprintf(`GOTCHA_AGENT_ENDPOINT=%s GOTCHA_AGENT_INGEST_KEY=%s sh -c "$(curl -fsSL %s/install.sh)"`,
		baseURL, key, baseURL)
}

// agentUpdateCommand — та же команда БЕЗ ключа и endpoint: install.sh,
// однажды запущенный на хосте, помнит их сам (файл окружения юнита), и
// повторный запуск того же скрипта переустанавливает бинарь агента поверх
// уже настроенного — обновление, а не повторная регистрация.
func agentUpdateCommand(baseURL string) string {
	return fmt.Sprintf(`sh -c "$(curl -fsSL %s/install.sh)"`, baseURL)
}

// parseSemverBase разбирает ведущий "vX.Y.Z"-префикс строки версии: срез
// префикса "v", затем три числовые группы вплоть до первого символа вне
// [0-9.] (суффикс вида "-5-gabcdef-dirty" у git-описания просто отбрасывается
// — сравнение версий агента/сервера идёт по базе релиза, спека §3.3). Любое
// отклонение от X.Y.Z (не три группы, нечисловая группа) — ok=false: вызывающая
// сторона (agentUpdateAvailable) в этом случае молчит, а не гадает.
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

// agentUpdateAvailable — версия агента строго старше версии сервера (по
// базе X.Y.Z, см. parseSemverBase). Любой невалидный семвер с любой стороны
// (агент ещё ни разу не отчитался, дев-сборка сервера без канона версии) —
// false: карточка молчит про обновление, а не показывает ложный бейдж.
// Агент новее сервера (staging/canary агент против отставшего prod-сервера)
// тоже false — не пугаем оператора несуществующим "устарел".
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
// host.Validate сравнивает результат с границами через <=/>= — сравнение с
// NaN всегда false в обе стороны ("NaN <= 0" и "NaN >= 1" одновременно
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

// hostThresholdsFormState — то же самое для формы «Пороги этого хоста»
// (hostThresholdsSave): режим (inherit/override/off) и введённое значение по
// каждому из 4 видов, для переотрисовки карточки после ошибки валидации.
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

// parseHostThresholdsForm разбирает форму «Пороги этого хоста» в
// ThresholdOverride (B2, T6): по каждому виду один из трёх режимов —
// "inherit" (оба указателя остаются nil — резолвер идёт дальше по каскаду),
// "override" (Enabled=true + распарсенное значение) или "off" (Enabled=false,
// значение не нужно — тот же контракт, что у ValidateOverride/host.Save).
// Любой другой/пустой режим трактуется как "inherit" (значение по умолчанию
// radio-группы при первом открытии формы). Числа проценты диска/памяти
// конвертируются в доли на этой же границе, что и parseHostSettingsForm;
// переполнение минут тишины отсекается тем же приёмом (граница ДО перевода в
// Duration).
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
		// Та же граница ДО перевода в Duration, что у parseHostSettingsForm
		// (переполнение int64 наносекунд на огромном числе минут).
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

// hostSettingsErrorMessage переводит сентинел-ошибки host.Validate (и
// parseHostSettingsForm/parseHostThresholdsForm/ValidateOverride — тот же
// набор сентинелов host.ErrInvalid*, см. hostThresholdsSave) в понятное
// сообщение — тот же приём, что
// maintenanceErrorMessage: errors.Is по каждому сентинелу вместо показа
// err.Error() (Go-текста "host: disk threshold must be in (0, 1): got 1.5")
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
// команда установки агента и конфиг коллектора под свёрнутыми блоками, плюс
// блок «Пороги по окружению/роли» (B2, T7) — список групповых правил проекта
// с модалками добавления/правки. form/errMsg — введённые пользователем
// значения формы ПРОЕКТНЫХ порогов при ошибке валидации (см. FormState);
// groupForm/groupErrMsg — то же самое, но для формы ГРУППОВОГО правила (два
// независимых блока на одной странице, ошибка одного не трогает другой).
func (h *Handler) renderHostSettings(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string, groupForm templates.FormState, groupErrMsg string) {
	settings, err := h.HostSettings.Get(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Групповые пороги — nil-safe, тем же приёмом, что GroupThresholds в
	// renderHostDetail (T6): main.go всегда проводит Hosts/GroupThresholds
	// вместе с HostSettings, но частично собранный Handler (тесты) не должен
	// падать — секция просто не покажет ни правил, ни формы добавления.
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
	// Какую модалку групповых порогов открыть с сервера. После 422
	// группового сохранения (groupForm != nil) — ту, из которой пришла
	// отправка: признак «это была правка» — существование правила с
	// отправленной парой scope+label (форма правки шлёт свою пару
	// hidden-полями). Отдельный маркер формы не нужен: Upsert идемпотентен
	// по паре, то есть отправка существующей пары И ЕСТЬ правка, из какой бы
	// модалки она ни началась, а пара правила, удалённого параллельно в
	// другой вкладке, честно падает в модалку создания — её поля читают те
	// же имена. На GET дежурит старый формат ссылки «Редактировать»
	// ?gt_scope=&gt_label= (закладки, переходы из писем): открывается
	// модалка правки найденного правила, а несуществующая пара — просто
	// страница без открытой модалки (не 404: правило могли удалить).
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

// groupThresholdFormState — как hostThresholdsFormState (переиспользует те
// же 8 полей per-вид disk/memory/load/silent), плюс scope/метка группового
// правила — для переотрисовки формы «Пороги по окружению/роли» после ошибки
// валидации (422) с теми же введёнными значениями (тот же приём FormState,
// что у hostSettingsFormState/hostThresholdsFormState).
func groupThresholdFormState(r *http.Request) templates.FormState {
	form := hostThresholdsFormState(r)
	form["scope"] = r.FormValue("scope")
	form["label_env"] = r.FormValue("label_env")
	form["label_role"] = r.FormValue("label_role")
	return form
}

// groupThresholdExists — есть ли среди правил проекта пара scope+label.
// Пустые значения — заведомо «нет», а не совпадение с пустой меткой (та же
// защита от «группы без метки», что у валидации hostGroupThresholdSave).
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
	h.renderHostSettings(w, r, http.StatusOK, projectID, nil, "", nil, "")
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
	// Форма порогов возвращается на саму себя с теми же значениями, что были
	// введены, — без flash сохранение выглядело как «ничего не произошло»
	// (UX-аудит A1, P1-5). Тот же приём, что у maintenance.go и alerts.go.
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostSettingsPath(projectID), http.StatusSeeOther)
}

// maxGroupThresholdLabelLen — верхняя граница длины label группового порога
// (it-sec P2-1 ремедиации): без неё оператор мог создать орфан-правило со
// сколь угодно длинной меткой — GroupThresholdService.List читает ВСЕ
// групповые пороги проекта на каждом тике оценщика (evaluator.go), и такая
// строка грузилась бы из PostgreSQL заново каждый Interval. Значение с
// большим запасом над любой реальной меткой окружения/роли из телеметрии.
const maxGroupThresholdLabelLen = 256

// hostGroupThresholdSave — POST /projects/{id}/hosts/settings/groups:
// сохранить групповой порог по scope (env/role) + label (значение метки из
// host.Store.FacetValues, B1). GroupThresholdService.Upsert идемпотентен по
// (project_id, scope, label) — сохранение под уже существующей парой
// scope+label ЗАМЕЩАЕТ её целиком (все 4 вида берутся из отправленной формы,
// как и per-host override, T6): это и есть «редактирование» — отдельного
// действия для него нет, формы модалок создания и правки шлют одни и те же
// поля на один роут (groupThresholdCreateModal/groupThresholdEditModal;
// какую модалку переоткрыть после 422 — см. renderHostSettings).
// Гейт — оператор + sameOrigin, тот же приём, что hostSettingsSave/
// hostThresholdsSave.
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
	// scope/label не проверяются на членство в текущих FacetValues (метка
	// могла исчезнуть между отрисовкой формы и отправкой) — орфан-правило
	// безвредно, резолвер просто не находит хостов с такой меткой (спека
	// §host_group_thresholds). Проверяется только форма: scope — одно из
	// двух известных значений, label — непусто (иначе UNIQUE-ключ (project_id,
	// scope, '') собирал бы несвязанные правила в одну строку).
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

// hostGroupThresholdDelete — POST /projects/{id}/hosts/settings/groups/delete:
// удалить групповое правило по scope+label (hidden-поля в форме строки
// таблицы, groupThresholdRow). Delete идемпотентен (GroupThresholdService.
// Delete, как и остальные стораджи продукта) — отсутствие строки не ошибка.
// Пустая пара scope/label — 422 на той же странице, первый POST без
// confirmed=yes — страница подтверждения. Гейт — оператор + sameOrigin.
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
	// Пустая или неизвестная пара scope/label — ошибка на той же странице
	// (K7-8), тем же ключом, что у hostGroupThresholdSave: раньше был голый
	// редирект — ни удаления, ни объяснения, почему правило на месте.
	if (scope != "env" && scope != "role") || label == "" {
		h.renderHostSettings(w, r, http.StatusUnprocessableEntity, projectID, nil,
			i18n.T(r.Context(), "error.hostsettings.group_scope_label"), nil, "")
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения, называющую группу (K7-7); пара
	// scope+label уезжает во второй POST hidden-полями, как channel_id у
	// alertsChannelDelete.
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
// процессы), открытые инциденты + недавняя история, блок «Пороги этого
// хоста» (B2, T6) и кнопка удаления (только оператору). Гейт —
// CanAccessProject, как у списка: карточку смотрит любой с доступом к
// проекту, мутирующие действия (пороги, удаление) требуют оператора отдельно
// (ниже, на кнопке/форме — CanOperate вычисляется здесь read-only, без гейта
// всей страницы). Несуществующее имя хоста → 404 (существование — часть
// проверки, чтобы страница не отдавала 200 на произвольный путь).
//
// Само построение VM вынесено в renderHostDetail — hostThresholdsSave
// переиспользует его для переотрисовки той же карточки с 422 и введённой
// формой при ошибке валидации (тот же приём, что renderHostSettings у
// hostSettingsSave).
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

// renderHostDetail строит VM карточки хоста и рендерит её со статусом status
// (200 у hostDetail, 422 у hostThresholdsSave при ошибке валидации формы
// порогов — thresholdForm/thresholdErr тогда непустые, см.
// hostSettingsFormState/renderHostSettings за тем же приёмом). uid уже
// проверен вызывающим (auth.UserID/requireProjectOperator) — здесь читается
// только для read-only CanOperate.
//
// eff — эффективные пороги ЭТОГО хоста (host.ThresholdResolver.Effective,
// каскад host-override → role-group → env-group → project → default,
// T3-T5): используются и в статус-бейдже/линиях графиков (hostRowStatus/
// hostDetailCharts — раньше брали только settings проекта, теперь честно
// учитывают per-host override), и в блоке «Пороги этого хоста» (показ
// текущего override + эффективного значения с источником).
func (h *Handler) renderHostDetail(w http.ResponseWriter, r *http.Request, status int, projectID, uid int64, hst host.Host, thresholdForm templates.FormState, thresholdErr string) {
	name := hst.Name

	projSettings, projExists, err := h.HostSettings.GetWithExists(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// GroupThresholds — nil-safe (T7 групповых порогов может ещё не быть
	// проведён вместе с HostOverrides, см. комментарий поля в web.go):
	// пустой список групп резолвер трактует как «групповых порогов нет»,
	// каскад просто идёт дальше к project/default.
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
	recentIncidents, err := h.hostRecentIncidents(r.Context(), projectID, hst.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	openKinds := make([]string, 0, len(openIncidents))
	for _, inc := range openIncidents {
		openKinds = append(openKinds, inc.Kind)
	}
	statusKind, problemKinds := hostRowStatus(openKinds, hst.LastSeen, time.Now(), eff.Settings)

	// Графики — из ClickHouse; отказ CH не роняет карточку хоста (единый
	// приём CH-страниц, образец — logsList): шапка, инциденты и действия
	// остаются, на месте графиков — «данные временно недоступны».
	charts, err := h.hostDetailCharts(r.Context(), projectID, name, from, to, step, eff.Settings)
	chartsFailed := err != nil
	if chartsFailed {
		slog.Warn("web: host charts failed", "project_id", projectID, "host", name, "error", err)
		charts = nil
	}

	// Аптайм — вспомогательный элемент шапки, не один из семи обязательных
	// графиков карточки: ошибка запроса не роняет всю страницу (как чарты
	// выше), а оставляет блок пустым — тем же принципом, что «нет ключа в
	// карте LatestByHost» ниже (хост ещё не отчитался uptime своим агентом
	// или коллектором). Окно — hostsListWindow, как у списка хостов (§5.2):
	// нужно самое свежее значение «сейчас», а не история за выбранный на
	// странице период vm.Range.
	var uptimeStr string
	now := time.Now()
	uptimeByHost, err := h.Metrics.LatestByHost(r.Context(), projectID, hostmetric.Uptime,
		nil, "", "max", now.Add(-hostsListWindow), now)
	if err != nil {
		slog.Warn("web: host uptime query failed", "project_id", projectID, "host", name, "error", err)
	} else if sec, ok := uptimeByHost[name]; ok {
		uptimeStr = humanize.Duration(r.Context(), time.Duration(sec*float64(time.Second)))
	}

	// CanOperate — read-only: считаем прямо здесь (не через requireProjectOperator,
	// который на отказе рендерит 404 всей странице) ровно как canManage у
	// MonitorDetail — просмотр карточки доступна всем с доступом к проекту,
	// кнопка удаления и форма порогов просто скрыты у не-оператора.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// AgentUpdateCmd — пусто, если путь агента недоступен (раздача не
	// сконфигурирована — sec-M1 — или BaseURL небезопасен — sec-M4): бейдж
	// "есть обновление" остаётся honest-сигналом (сверен по версии,
	// hst.AgentVersion пришла от самого агента), а готовую команду карточка
	// не предлагает, если её нельзя безопасно исполнить.
	agentUpdateCmd := ""
	if h.agentDistAvailable() && agentBaseURLSecure(h.BaseURL) {
		agentUpdateCmd = agentUpdateCommand(h.BaseURL)
	}

	// serverVersion — цель сравнения AgentUpdateAvailable и, отдельно,
	// подпись блока "Как обновить агента до {version}" (rem-E ux-L16):
	// бейдж "Есть обновление" сам не называл, до какой версии.
	serverVersion := version.Version()

	// ackedBy — W2-C находка 4: email подтвердившего, батчем по открытым
	// инцидентам (см. ackedByEmails).
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

// hostThresholdsSave — POST /projects/{id}/hosts/{name}/thresholds:
// сохранить per-host override порогов инцидентов (B2, T6) поверх каскада
// host→role→env→project→default (ThresholdResolver, resolve.go). Гейт —
// оператор + sameOrigin, тот же приём, что hostSettingsSave. Ошибка
// валидации (значение вне границы, нечисловой ввод ИЛИ "override" без
// значения/"выключено" с числом вне границ — ValidateOverride) → 422 с
// переотрисовкой карточки хоста и введёнными значениями формы
// (hostThresholdsFormState), как у hostSettingsSave/renderHostSettings.
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
	// HostOverrideService.Save зовёт ValidateOverride сам (override.go) —
	// отдельного вызова здесь не нужно; ошибка — тот же набор сентинелов
	// host.ErrInvalid*, что у parseHostThresholdsForm/parseHostSettingsForm.
	if err := h.HostOverrides.Save(r.Context(), hst.ID, ov); err != nil {
		if errors.Is(err, host.ErrInvalidDiskThreshold) || errors.Is(err, host.ErrInvalidMemoryThreshold) ||
			errors.Is(err, host.ErrInvalidLoadThreshold) || errors.Is(err, host.ErrInvalidSilentAfter) {
			h.renderHostDetail(w, r, http.StatusUnprocessableEntity, projectID, uid, hst, hostThresholdsFormState(r), hostSettingsErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Карточка возвращается на саму себя — без flash сохранение выглядело бы
	// как «ничего не произошло» (тот же UX-урок P1-5, что у hostSettingsSave).
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, hostDetailPath(projectID, name), http.StatusSeeOther)
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
	if !h.parseForm(w, r) {
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
