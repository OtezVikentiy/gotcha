package web

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"gopkg.in/yaml.v3"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// TestHostPathHelpers — построители путей: чистые строковые функции,
// покрываются прямым вызовом (как metricDetailURL и соседи в webhelpers_test.go).
func TestHostPathHelpers(t *testing.T) {
	cases := []struct{ got, want string }{
		{hostsPath(7), "/projects/7/hosts"},
		{hostSettingsPath(7), "/projects/7/hosts/settings"},
		{hostDetailPath(7, "web-1"), "/projects/7/hosts/web-1"},
		// имя хоста с символами, требующими экранирования пути
		{hostDetailPath(7, "a b/c"), "/projects/7/hosts/a%20b%2Fc"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("path = %q, want %q", c.got, c.want)
		}
	}
}

// TestSortHostRows — проблемные хосты сверху, затем тихие, затем ok; внутри
// каждой группы — по имени (§5.2 дизайна).
func TestSortHostRows(t *testing.T) {
	rows := []templates.HostRowVM{
		{Name: "z-ok", StatusKind: "ok"},
		{Name: "b-problem", StatusKind: "problem"},
		{Name: "a-silent", StatusKind: "silent"},
		{Name: "a-ok", StatusKind: "ok"},
		{Name: "a-problem", StatusKind: "problem"},
		{Name: "z-silent", StatusKind: "silent"},
	}
	sortHostRows(rows)
	want := []string{"a-problem", "b-problem", "a-silent", "z-silent", "a-ok", "z-ok"}
	for i, name := range want {
		if rows[i].Name != name {
			t.Fatalf("rows[%d] = %q, want %q (order: %v)", i, rows[i].Name, name, rows)
		}
	}
}

// TestHostRowStatus — классификация тира строки (ревью T14, находка 2):
// kind="silent" среди открытых инцидентов не должен попадать в "problem" —
// тишина, обнаруженная host.Evaluator'ом (открытый incident kind="silent") и
// тишина, посчитанная здесь же по last_seen, обязаны давать ОДИН И ТОТ ЖЕ
// тир "silent", иначе бейдж хоста мерцает между "Тихий" и "Тишина" ровно на
// тике оценщика без изменения реального состояния хоста.
func TestHostRowStatus(t *testing.T) {
	now := time.Now()
	settings := host.Settings{SilentEnabled: true, SilentAfter: 5 * time.Minute}

	cases := []struct {
		name        string
		openKinds   []string
		lastSeen    time.Time
		wantKind    string
		wantProblem []string
	}{
		{"ok: свежий, без инцидентов", nil, now, "ok", nil},
		{"problem: один вид", []string{"disk"}, now, "problem", []string{"disk"}},
		{"problem: несколько видов, silent среди них не в OpenKinds",
			[]string{"disk", "silent", "load"}, now, "problem", []string{"disk", "load"}},
		{"silent по инциденту, last_seen свежий (не должен быть problem)",
			[]string{"silent"}, now, "silent", nil},
		{"silent по last_seen, без инцидентов",
			nil, now.Add(-10 * time.Minute), "silent", nil},
		{"silent по last_seen, свежий last_seen → ok",
			nil, now, "ok", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, problem := hostRowStatus(c.openKinds, c.lastSeen, now, settings)
			if kind != c.wantKind {
				t.Errorf("kind = %q, want %q", kind, c.wantKind)
			}
			if len(problem) != len(c.wantProblem) {
				t.Fatalf("problemKinds = %v, want %v", problem, c.wantProblem)
			}
			for i := range problem {
				if problem[i] != c.wantProblem[i] {
					t.Errorf("problemKinds = %v, want %v", problem, c.wantProblem)
				}
			}
		})
	}
}

// TestHostRowStatusSilentDisabled — SilentEnabled=false: last_seen устаревший
// сколько угодно не должен давать "silent" (порог выключен), только открытый
// incident kind="silent" — тир силы источника детекции разные, но правило
// «silent — один тир» держится и здесь.
func TestHostRowStatusSilentDisabled(t *testing.T) {
	now := time.Now()
	settings := host.Settings{SilentEnabled: false, SilentAfter: 5 * time.Minute}

	kind, _ := hostRowStatus(nil, now.Add(-time.Hour), now, settings)
	if kind != "ok" {
		t.Errorf("SilentEnabled=false, last_seen устарел, нет инцидентов: kind = %q, want ok", kind)
	}
	kind, _ = hostRowStatus([]string{"silent"}, now, now, settings)
	if kind != "silent" {
		t.Errorf("SilentEnabled=false, но есть открытый incident kind=silent: kind = %q, want silent", kind)
	}
}

// TestCollectorConfig — конфиг коллектора (§5.4 дизайна) содержит endpoint
// БЕЗ /v1/metrics (путь дописывает сам otlphttp-экспортёр), Bearer-заголовок
// с публичным ключом, явно включённую system.cpu.logical.count (делитель
// load/core, §4.1 — без неё порог load никогда не считается) и все три
// *.utilization-метрики (cpu/memory/filesystem), нужные графикам и
// встроенным порогам диска/памяти.
func TestCollectorConfig(t *testing.T) {
	cfg := collectorConfig("https://g.example", "pubkey")

	if !strings.Contains(cfg, "endpoint: https://g.example") {
		t.Errorf("нет endpoint без пути: %s", cfg)
	}
	if strings.Contains(cfg, "https://g.example/v1/metrics") {
		t.Errorf("endpoint не должен содержать /v1/metrics (путь дописывает otlphttp): %s", cfg)
	}
	if !strings.Contains(cfg, `Authorization: "Bearer pubkey"`) {
		t.Errorf("нет заголовка Authorization: Bearer <ключ>: %s", cfg)
	}
	if !strings.Contains(cfg, "system.cpu.logical.count: {enabled: true}") {
		t.Errorf("нет явного включения system.cpu.logical.count (делитель load/core): %s", cfg)
	}
	if got := strings.Count(cfg, "utilization: {enabled: true}"); got != 3 {
		t.Errorf("utilization-метрик включено %d, want 3 (cpu/memory/filesystem): %s", got, cfg)
	}
	for _, scraper := range []string{"cpu:", "memory:", "filesystem:", "disk: {}", "network: {}", "load: {}", "processes: {}"} {
		if !strings.Contains(cfg, scraper) {
			t.Errorf("нет скрейпера %q: %s", scraper, cfg)
		}
	}
}

// filesystemScraperConfig разбирает конфиг как YAML и возвращает секцию
// скрейпера filesystem. Разбор, а не strings.Contains: конфиг отдаётся
// пользователю на копирование в /etc/otelcol-contrib/config.yaml, и «строка
// присутствует» ничего не говорит о том, что otelcol его прочитает —
// многострочные flow-списки исключений сломать отступом легко.
func filesystemScraperConfig(t *testing.T, cfg string) map[string]any {
	t.Helper()
	var parsed struct {
		Receivers struct {
			Hostmetrics struct {
				Scrapers map[string]any `yaml:"scrapers"`
			} `yaml:"hostmetrics"`
		} `yaml:"receivers"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("конфиг коллектора не разбирается как YAML: %v\n%s", err, cfg)
	}
	fs, ok := parsed.Receivers.Hostmetrics.Scrapers["filesystem"].(map[string]any)
	if !ok {
		t.Fatalf("в конфиге нет секции scrapers.filesystem: %s", cfg)
	}
	return fs
}

// stringList достаёт из секции исключений список значений по ключу
// (fs_types/mount_points), проверяя заодно match_type — без него otelcol
// применил бы strict по умолчанию, и регулярные выражения стали бы точными
// именами точек монтирования, то есть исключение перестало бы работать молча.
func stringList(t *testing.T, section map[string]any, key, listKey, wantMatchType string) []string {
	t.Helper()
	sub, ok := section[key].(map[string]any)
	if !ok {
		t.Fatalf("нет секции %s: %+v", key, section)
	}
	if got, _ := sub["match_type"].(string); got != wantMatchType {
		t.Fatalf("%s.match_type = %q, want %q", key, got, wantMatchType)
	}
	raw, ok := sub[listKey].([]any)
	if !ok {
		t.Fatalf("%s.%s не список: %+v", key, listKey, sub)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s.%s содержит не строку: %v", key, listKey, v)
		}
		out = append(out, s)
	}
	return out
}

// TestCollectorConfigExcludesPseudoFilesystems — ревью I1: скрейпер filesystem
// без исключений собирает ВСЕ смонтированные ФС, а встроенный порог диска
// берёт максимум по mountpoint'ам. На обычной Ubuntu каждый snap смонтирован
// squashfs'ом, заполненным на 100% по замыслу, — дефолтный порог «>90%»
// открывал бы инцидент на первом же тике оценщика, закрыть который нечем
// (диск свободен), а топ-8 графика занятости состоял бы из /snap/*.
func TestCollectorConfigExcludesPseudoFilesystems(t *testing.T) {
	cfg := collectorConfig("https://g.example", "pubkey")
	fs := filesystemScraperConfig(t, cfg)

	fsTypes := stringList(t, fs, "exclude_fs_types", "fs_types", "strict")
	for _, want := range []string{"squashfs", "tmpfs", "devtmpfs", "overlay", "iso9660", "autofs", "proc", "sysfs"} {
		if !slices.Contains(fsTypes, want) {
			t.Errorf("тип ФС %q не исключён: %v", want, fsTypes)
		}
	}

	mounts := stringList(t, fs, "exclude_mount_points", "mount_points", "regexp")
	for _, want := range []string{"^/snap/.*", "^/var/lib/docker/.*", "^/run/.*"} {
		if !slices.Contains(mounts, want) {
			t.Errorf("точка монтирования %q не исключена: %v", want, mounts)
		}
	}
	for _, expr := range mounts {
		re, err := regexp.Compile(expr)
		if err != nil {
			t.Errorf("шаблон %q не компилируется как регулярное выражение: %v", expr, err)
			continue
		}
		// filterset/regexp матчит ПОДСТРОКУ — без якоря "^/run/.*" поймал бы
		// и /mnt/backup/run/x; якорь входит в сам шаблон, проверяем его делом.
		if re.MatchString("/mnt/backup" + strings.TrimPrefix(strings.TrimSuffix(expr, ".*"), "^")) {
			t.Errorf("шаблон %q не заякорен: срабатывает на вложенном пути", expr)
		}
	}

	// Метрика занятости остаётся включённой — исключения не должны были
	// вытеснить секцию metrics того же скрейпера.
	metrics, ok := fs["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("исключения вытеснили секцию metrics скрейпера filesystem: %+v", fs)
	}
	if _, ok := metrics["system.filesystem.utilization"]; !ok {
		t.Errorf("system.filesystem.utilization не включена: %+v", metrics)
	}
}

// TestCollectorConfigGeneratedLists — списки исключений YAML рендерятся из
// internal/hostmetric, а не дублируются строкой в шаблоне: правка исключений
// в одном месте меняет и агент, и конфиг коллектора. Плюс паритет путей —
// system.uptime едет и с самого коллектора (§3.4 спеки), не только с агента.
func TestCollectorConfigGeneratedLists(t *testing.T) {
	cfg := collectorConfig("https://g.example", "pk_x")
	// Списки исключений рендерятся из hostmetric — источник один.
	for _, fs := range hostmetric.ExcludedFSTypes {
		if !strings.Contains(cfg, fs) {
			t.Errorf("нет fs-типа %q в YAML", fs)
		}
	}
	for _, p := range hostmetric.ExcludedMountPrefixes {
		if !strings.Contains(cfg, "^"+p+".*") {
			t.Errorf("нет регэкспа маунта для %q", p)
		}
	}
	// Паритет путей: uptime едет и с коллектора (§3.4 спеки).
	if !strings.Contains(cfg, "system:") || !strings.Contains(cfg, "system.uptime: {enabled: true}") {
		t.Error("system-scraper с system.uptime не включён")
	}
}

// TestAgentUpdateAvailable — сравнение версий по базе X.Y.Z (спека §3.3):
// префикс "v" срезается, суффикс после третьей числовой группы (dev-сборка
// "-N-gHASH") игнорируется, любой невалидный семвер (пустая строка, мусор,
// "dev" у сервера) даёт false — молчим, а не пугаем оператора ложным
// бейджем. Агент новее сервера — тоже false: обновление не предлагаем
// откатить.
func TestAgentUpdateAvailable(t *testing.T) {
	cases := []struct {
		agent, server string
		want          bool
	}{
		{"0.5.0", "0.6.0", true},
		{"v0.5.0", "0.6.0", true},        // префикс v срезается
		{"0.6.0", "0.6.0", false},        // версии совпадают
		{"0.6.1", "0.6.0", false},        // агент новее — не пугаем
		{"0.6.0-5-gabc", "0.6.0", false}, // сравнение по базе X.Y.Z
		{"", "0.6.0", false},             // нет данных — молчим
		{"мусор", "0.6.0", false},
		{"0.6.0", "dev", false},   // сервер без валидного семвера — молчим
		{"0.9.0", "0.10.0", true}, // числовое сравнение minor, не лексикографическое ("9" > "10" строкой)
		{"0.10.0", "0.9.0", false},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.server, func(t *testing.T) {
			if got := agentUpdateAvailable(c.agent, c.server); got != c.want {
				t.Errorf("agentUpdateAvailable(%q, %q) = %v, want %v", c.agent, c.server, got, c.want)
			}
		})
	}
}

// TestAgentCommands — команда установки несёт ключ и endpoint (DSN-эквивалент
// хостовых метрик), команда обновления — БЕЗ ключа (повторный запуск того же
// install.sh переустанавливает бинарь агента, а не выпускает новый ключ).
// Обе загружают install.sh полностью перед исполнением (`sh -c "$(curl ...)"`,
// не `curl | sh`) — симметрия форм, см. докблок задачи.
func TestAgentCommands(t *testing.T) {
	install := agentInstallCommand("https://g.example", "pk_x")
	if !strings.Contains(install, "https://g.example/install.sh") {
		t.Errorf("install-команда без /install.sh: %s", install)
	}
	if !strings.Contains(install, "GOTCHA_AGENT_KEY=pk_x") {
		t.Errorf("install-команда без ключа: %s", install)
	}
	if !strings.Contains(install, "GOTCHA_AGENT_ENDPOINT=https://g.example") {
		t.Errorf("install-команда без endpoint: %s", install)
	}

	update := agentUpdateCommand("https://g.example")
	if !strings.Contains(update, "https://g.example/install.sh") {
		t.Errorf("update-команда без /install.sh: %s", update)
	}
	if strings.Contains(update, "GOTCHA_AGENT_KEY") {
		t.Errorf("update-команда не должна нести ключ: %s", update)
	}
}

// renderHostDetail — прямой рендер templ-компонента карточки хоста (как
// renderTo в templates-пакете), без HTTP-стенда: этому тесту важна только
// разметка шапки по готовой VM.
func renderHostDetail(t *testing.T, vm templates.HostDetailVM) string {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := templates.HostDetail(vm, "").Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestHostDetailShowsAgentVersion — версия агента и бейдж обновления в шапке
// карточки: при заполненной AgentVersion и доступном обновлении в HTML есть
// сама версия, i18n-текст ключа "hosts.detail.agent_update" и команда
// обновления (T13); при AgentVersion=="" — ни строки версии, ни бейджа (нет
// данных, а не «версия неизвестна»).
func TestHostDetailShowsAgentVersion(t *testing.T) {
	base := templates.HostDetailVM{
		Host: host.Host{Name: "web-1"},
	}

	withVersion := base
	withVersion.Host.AgentVersion = "0.5.0"
	withVersion.AgentVersion = "0.5.0"
	withVersion.AgentUpdateAvailable = true
	withVersion.AgentUpdateCmd = agentUpdateCommand("https://g.example")
	html := renderHostDetail(t, withVersion)

	if !strings.Contains(html, "0.5.0") {
		t.Errorf("нет версии агента в разметке: %s", html)
	}
	wantBadge := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "hosts.detail.agent_update")
	if !strings.Contains(html, wantBadge) {
		t.Errorf("нет текста бейджа обновления (%q): %s", wantBadge, html)
	}
	if !strings.Contains(html, "install.sh") {
		t.Errorf("нет команды обновления агента: %s", html)
	}

	empty := base // AgentVersion пуст
	html = renderHostDetail(t, empty)
	if strings.Contains(html, wantBadge) {
		t.Errorf("бейдж обновления показан без версии агента: %s", html)
	}
	if strings.Contains(html, "hosts.detail.agent_version") {
		t.Errorf("сырой i18n-ключ версии агента в разметке: %s", html)
	}
}

// TestHostDetailAgentUpdateShowsTargetVersion — rem-E ux-L16: блок «Как
// обновить агента» должен называть версию сервера, до которой пойдёт
// обновление, — бейдж «Есть обновление» сам по себе цель не называет.
func TestHostDetailAgentUpdateShowsTargetVersion(t *testing.T) {
	vm := templates.HostDetailVM{
		Host:                 host.Host{Name: "web-1", AgentVersion: "0.5.0"},
		AgentVersion:         "0.5.0",
		AgentUpdateAvailable: true,
		AgentUpdateCmd:       agentUpdateCommand("https://g.example"),
		ServerVersion:        "0.6.0",
	}
	html := renderHostDetail(t, vm)

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantHowto := i18n.Tf(ctx, "hosts.detail.agent_update_howto", "version", "0.6.0")
	if !strings.Contains(html, wantHowto) {
		t.Errorf("блок обновления не называет целевую версию (%q): %s", wantHowto, html)
	}
}

// renderHostsListOnboarding — прямой рендер онбординга пустого списка хостов
// (как renderHostDetail): важна только структура блока «Установить агент» +
// свёрнутая альтернатива коллектора, не сам список.
func renderHostsListOnboarding(t *testing.T, installCmd, config, agentReason string) string {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := templates.HostsList(1, nil, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, installCmd, config, agentReason, "").Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestHostsOnboardingAgentDefault — T14: онбординг предлагает свой агент по
// умолчанию (одна install.sh-команда с ключом+endpoint), коллектор otelcol —
// свёрнутая альтернатива с прежними тремя шагами. Оба пути берут ключ одним
// и тем же firstLiveKey-путём (h.hostInstallBlocks в hosts.go), поэтому при
// его отсутствии подсказка hosts.onboarding.no_key
// показывается на ОБОИХ путях, а не на одном.
func TestHostsOnboardingAgentDefault(t *testing.T) {
	installCmd := agentInstallCommand("https://g.example", "pk_x")
	config := collectorConfig("https://g.example", "pk_x")
	html := renderHostsListOnboarding(t, installCmd, config, "")

	for _, want := range []string{"GOTCHA_AGENT_ENDPOINT=https://g.example", "GOTCHA_AGENT_KEY=pk_x", "/install.sh"} {
		if !strings.Contains(html, want) {
			t.Errorf("онбординг без фрагмента команды агента %q: %s", want, html)
		}
	}
	if !strings.Contains(html, "<details") {
		t.Errorf("нет свёрнутого блока альтернативы коллектора: %s", html)
	}
	if !strings.Contains(html, "otlphttp") { // фрагмент YAML коллектора внутри details
		t.Errorf("свёрнутый блок без конфига коллектора: %s", html)
	}

	noKeyHTML := renderHostsListOnboarding(t, "", "", "")
	wantHint := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "hosts.onboarding.no_key")
	if got := strings.Count(noKeyHTML, wantHint); got != 2 {
		t.Errorf("подсказка no_key встречается %d раз(а) без ключа, want 2 (агент-шаг + коллектор-альтернатива): %s", got, noKeyHTML)
	}
}

// TestHostsOnboardingAgentDocsLink — rem-E ux-M7: на пути агента по
// умолчанию (вне свёрнутой коллектор-альтернативы) должна быть своя ссылка
// на /docs/hosts и подсказка про поддерживаемые платформы — до этой правки
// на дефолтном пути не было ни того ни другого, обе жили только внутри
// свёрнутого блока коллектора.
func TestHostsOnboardingAgentDocsLink(t *testing.T) {
	installCmd := agentInstallCommand("https://g.example", "pk_x")
	config := collectorConfig("https://g.example", "pk_x")
	html := renderHostsListOnboarding(t, installCmd, config, "")

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantPlatforms := i18n.T(ctx, "hosts.onboarding.agent_platforms")
	if !strings.Contains(html, wantPlatforms) {
		t.Errorf("нет подсказки про платформы на шаге агента (%q): %s", wantPlatforms, html)
	}
	if got := strings.Count(html, `href="/docs/hosts"`); got != 3 {
		t.Errorf("ссылка на /docs/hosts встречается %d раз(а), want 3 (help-панель + агент-шаг + коллектор-альтернатива): %s", got, html)
	}
}

// TestHostsOnboardingNoKeyLinksToSettings — rem-E ux-M4: без активного
// публичного ключа проекта подсказка no_key обязана вести на страницу
// настроек проекта (/projects/{id}/settings), где ключ и заводится, а не
// оставлять читателя угадывать путь.
func TestHostsOnboardingNoKeyLinksToSettings(t *testing.T) {
	html := renderHostsListOnboarding(t, "", "", "")

	wantHref := `href="/projects/1/settings"`
	if got := strings.Count(html, wantHref); got != 2 {
		t.Errorf("ссылка на настройки проекта встречается %d раз(а), want 2 (агент-шаг + коллектор-альтернатива): %s", got, html)
	}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantLinkText := i18n.T(ctx, "hosts.onboarding.no_key_link")
	if !strings.Contains(html, wantLinkText) {
		t.Errorf("нет текста ссылки на настройки (%q): %s", wantLinkText, html)
	}
}

// TestHostsOnboardingAgentUnavailable — rem-A sec-M1: ключ есть (коллектор
// заполнен), но раздача бинарей агента недоступна (agentDistAvailable()
// ложен на инстансе, собранном не из Docker-образа) — онбординг не должен
// предлагать install.sh-команду, которая гарантированно упрётся в 404, а
// обязан объяснить причину явно.
func TestHostsOnboardingAgentUnavailable(t *testing.T) {
	config := collectorConfig("https://g.example", "pk_x")
	html := renderHostsListOnboarding(t, "", config, "dist")

	if strings.Contains(html, "curl") || strings.Contains(html, "/install.sh") {
		t.Errorf("онбординг предлагает команду агента при недоступной раздаче: %s", html)
	}
	wantHint := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "hosts.onboarding.agent_unavailable")
	if !strings.Contains(html, wantHint) {
		t.Errorf("нет подсказки о недоступной раздаче (%q): %s", wantHint, html)
	}
	if !strings.Contains(html, "otlphttp") {
		t.Errorf("коллектор-альтернатива должна остаться заполненной: %s", html)
	}
}

// TestHostsOnboardingAgentInsecure — rem-A sec-M4: BaseURL не https:// и не
// локальный — онбординг не должен предлагать root-команду по каналу,
// уязвимому MITM, и обязан явно предупредить про HTTPS.
func TestHostsOnboardingAgentInsecure(t *testing.T) {
	config := collectorConfig("http://gotcha.example", "pk_x")
	html := renderHostsListOnboarding(t, "", config, "insecure")

	if strings.Contains(html, "curl") || strings.Contains(html, "/install.sh") {
		t.Errorf("онбординг предлагает команду агента по незащищённому каналу: %s", html)
	}
	wantHint := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "hosts.onboarding.agent_insecure")
	if !strings.Contains(html, wantHint) {
		t.Errorf("нет предупреждения про HTTPS (%q): %s", wantHint, html)
	}
	if !strings.Contains(html, "otlphttp") {
		t.Errorf("коллектор-альтернатива должна остаться заполненной: %s", html)
	}
}

// TestHostsListFiltersRendersChipsAndRows — B1, T5: фильтр env/role/new
// рендерит фасет-чипы (значения + сентинел «без метки»), активное значение
// отмечено, отфильтрованные строки несут свои env/role-бейджи. Рендерится
// VM напрямую (rows/filter/facets, а не через хендлер+стор) — тот же приём,
// что у renderHostDetail/renderHostsListOnboarding по соседству.
func TestHostsListFiltersRendersChipsAndRows(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{
		{Name: "web-1", StatusKind: "ok", Environment: "prod", Role: "web"},
	}
	filter := templates.HostsFilterVM{Environment: "prod", Active: true}
	facets := templates.NewHostsFacets(rctx, 1, filter, []string{"prod", "staging"}, []string{"web", "db"})

	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, filter, facets, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	// Активное значение фасета — ссылка со сбросом (env=prod уже выбран,
	// повторный клик снимает фильтр: href БЕЗ env=).
	if !strings.Contains(html, `class="chip is-active"`) {
		t.Errorf("нет активного чипа фасета: %s", html)
	}
	if !strings.Contains(html, "href=\"/projects/1/hosts?env=prod&amp;role=web\"") {
		t.Errorf("нет ссылки-тоггла на значение фасета role=web: %s", html)
	}
	// Сентинел «без метки» присутствует в обоих фасетах.
	wantNoneLabel := i18n.T(rctx, "hosts.label.none")
	if got := strings.Count(html, wantNoneLabel); got != 2 {
		t.Errorf("сентинел «без метки» встречается %d раз(а), want 2 (env+role): %s", got, html)
	}
	if !strings.Contains(html, "role=__none__") {
		t.Errorf("ссылка сентинела не несёт __none__: %s", html)
	}
	// Отфильтрованная строка со своими бейджами env/role.
	if !strings.Contains(html, `<a href="/projects/1/hosts/web-1">web-1</a>`) {
		t.Errorf("нет строки отфильтрованного хоста web-1: %s", html)
	}
	if !strings.Contains(html, `<span class="badge badge-neutral">prod</span>`) {
		t.Errorf("нет бейджа environment=prod у строки: %s", html)
	}
	if !strings.Contains(html, `<span class="badge badge-neutral">web</span>`) {
		t.Errorf("нет бейджа role=web у строки: %s", html)
	}
	// Активный фильтр — ссылка полного сброса.
	wantReset := i18n.T(rctx, "hosts.filter.reset")
	if !strings.Contains(html, wantReset) {
		t.Errorf("нет ссылки сброса фильтра при активном фильтре: %s", html)
	}
}

// TestHostNewBadgeBoundary — граница hostNewWindow (24ч, T5): хост с
// first_seen 23ч назад показывает бейдж «новый» (и в строке списка, и в
// шапке карточки), хост с first_seen 25ч назад — не показывает. Правило
// IsNew то же самое, что SQL-ветка HostFilter.NewOnly (host_test.go) —
// здесь проверяется только рендер бейджа, а не сам подсчёт границы.
func TestHostNewBadgeBoundary(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantBadge := i18n.T(rctx, "hosts.badge.new")
	now := time.Now()

	rows := []templates.HostRowVM{
		{Name: "web-1", StatusKind: "ok", IsNew: now.Sub(now.Add(-23*time.Hour)) < hostNewWindow},
		{Name: "web-2", StatusKind: "ok", IsNew: now.Sub(now.Add(-25*time.Hour)) < hostNewWindow},
		// web-3 — ровно на границе (first_seen ровно 24ч назад): сравнение
		// строгое (<), поэтому IsNew=false, как и у 25ч-хоста.
		{Name: "web-3", StatusKind: "ok", IsNew: now.Sub(now.Add(-24*time.Hour)) < hostNewWindow},
	}
	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	if got := strings.Count(html, wantBadge); got != 1 {
		t.Errorf("бейдж «новый» встречается %d раз(а) в списке, want 1 (только у 23ч-хоста): %s", got, html)
	}

	newDetail := renderHostDetail(t, templates.HostDetailVM{Host: host.Host{Name: "web-1"}, IsNew: true})
	if !strings.Contains(newDetail, wantBadge) {
		t.Errorf("нет бейджа «новый» в карточке при IsNew=true: %s", newDetail)
	}
	oldDetail := renderHostDetail(t, templates.HostDetailVM{Host: host.Host{Name: "web-1"}, IsNew: false})
	if strings.Contains(oldDetail, wantBadge) {
		t.Errorf("бейдж «новый» показан в карточке при IsNew=false: %s", oldDetail)
	}
}

// TestHostsListFilterEmptyShowsResetNotOnboarding — под фильтром, давшим
// пустой результат, список НЕ должен показывать онбординг «Хостов пока
// нет» (проект не пуст — просто ни один хост не подошёл под фильтр) — иначе
// текст с готовностью установить агента вводит в заблуждение владельца,
// у которого хосты уже есть.
func TestHostsListFilterEmptyShowsResetNotOnboarding(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	filter := templates.HostsFilterVM{Environment: "nowhere", Active: true}
	facets := templates.NewHostsFacets(rctx, 1, filter, []string{"prod"}, []string{"web"})

	var sb strings.Builder
	if err := templates.HostsList(1, nil, false, hostsListLimit, filter, facets, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	wantEmptyFilterTitle := i18n.T(rctx, "hosts.empty.filter.title")
	if !strings.Contains(html, wantEmptyFilterTitle) {
		t.Errorf("нет заголовка «под фильтр ничего не подошло»: %s", html)
	}
	wantOnboardingTitle := i18n.T(rctx, "hosts.empty.title")
	if strings.Contains(html, wantOnboardingTitle) {
		t.Errorf("показан онбординг «хостов пока нет» вместо сброса фильтра: %s", html)
	}
}

// TestGroupHostRows — группировка строк списка по env/role (T6): пустое
// значение метки уходит в отдельную секцию «(без метки)», секции
// сортируются по итоговому (локализованному) label, порядок строк внутри
// секции сохраняется от sortHostRows (группировка режет уже отсортированный
// список, не переупорядочивает).
func TestGroupHostRows(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{
		{Name: "b-staging", Environment: "staging"},
		{Name: "a-prod", Environment: "prod"},
		{Name: "c-none", Environment: ""},
		{Name: "b-prod", Environment: "prod"},
	}

	sections := groupHostRows(rctx, rows, "env")

	wantNone := i18n.T(rctx, "hosts.label.none")
	if len(sections) != 3 {
		t.Fatalf("секций %d, want 3: %+v", len(sections), sections)
	}
	wantLabels := []string{wantNone, "prod", "staging"}
	for i, want := range wantLabels {
		if sections[i].Label != want {
			t.Errorf("sections[%d].Label = %q, want %q (все: %+v)", i, sections[i].Label, want, sections)
		}
	}
	prodSection := sections[1]
	if len(prodSection.Rows) != 2 || prodSection.Rows[0].Name != "a-prod" || prodSection.Rows[1].Name != "b-prod" {
		t.Errorf("секция prod = %+v, want [a-prod, b-prod] в исходном порядке", prodSection.Rows)
	}
	noneSection := sections[0]
	if len(noneSection.Rows) != 1 || noneSection.Rows[0].Name != "c-none" {
		t.Errorf("секция «без метки» = %+v, want [c-none]", noneSection.Rows)
	}

	// group == "" (по умолчанию, без группировки) — сечений нет вовсе.
	if got := groupHostRows(rctx, rows, ""); got != nil {
		t.Errorf("groupHostRows с group=\"\" = %+v, want nil", got)
	}
}

// TestHostsListGroupRendersSections — HostsList с group=env рендерит строки
// секциями (заголовок = label секции) вместо плоской таблицы, а переключатель
// группировки сохраняет активный фильтр env/role/new в ссылках сегментов
// (T6: «фильтр и группировка компонуются»).
func TestHostsListGroupRendersSections(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{
		{Name: "web-1", StatusKind: "ok", Environment: "prod"},
		{Name: "web-2", StatusKind: "ok", Environment: ""},
	}
	filter := templates.HostsFilterVM{Role: "web", Active: true, Group: "env"}
	facets := templates.NewHostsFacets(rctx, 1, filter, []string{"prod"}, []string{"web"})
	sections := groupHostRows(rctx, rows, "env")

	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, filter, facets, sections, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	headerRe := regexp.MustCompile(`<h2 class="card-header">([^<]*)</h2>`)
	var headers []string
	for _, m := range headerRe.FindAllStringSubmatch(html, -1) {
		headers = append(headers, m[1])
	}
	wantNone := i18n.T(rctx, "hosts.label.none")
	if want := []string{wantNone, "prod"}; len(headers) != len(want) || headers[0] != want[0] || headers[1] != want[1] {
		t.Errorf("заголовки секций = %v, want %v", headers, want)
	}
	for _, want := range []string{"web-1", "web-2"} {
		if !strings.Contains(html, want) {
			t.Errorf("нет строки %q в сгруппированном рендере: %s", want, html)
		}
	}
	// Переключатель группировки: активный сегмент "По окружению", ссылка на
	// "По роли" сохраняет role=web (уже активный фильтр).
	wantActiveEnv := i18n.T(rctx, "hosts.group.env")
	if !strings.Contains(html, `aria-current="page">`+wantActiveEnv+`</a>`) {
		t.Errorf("сегмент «по окружению» не отмечен активным: %s", html)
	}
	if !strings.Contains(html, `href="/projects/1/hosts?group=role&amp;role=web"`) {
		t.Errorf("ссылка переключения на group=role не сохраняет role=web: %s", html)
	}
	if !strings.Contains(html, `href="/projects/1/hosts?role=web"`) {
		t.Errorf("ссылка «без группировки» не сохраняет role=web и не убирает group=: %s", html)
	}
}

// renderHostSettingsPage — прямой рендер страницы настроек порогов.
func renderHostSettingsPage(t *testing.T, installCmd, config, agentReason string) string {
	t.Helper()
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := templates.HostSettings(1, host.DefaultSettings(), installCmd, config, agentReason, nil, "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestHostsTableStatusKinds — добор покрытия TEMPL (hostsTable): все три
// ветки switch по row.StatusKind ("problem" с несколькими OpenKinds — ветка
// разделителя ", " при i>0, "silent", default "ok") в одном рендере.
func TestHostsTableStatusKinds(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{
		{Name: "p-1", StatusKind: "problem", OpenKinds: []string{"disk", "load"}},
		{Name: "s-1", StatusKind: "silent"},
		{Name: "o-1", StatusKind: "ok"},
	}
	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	wantDisk := i18n.T(rctx, "hosts.kind.disk")
	wantLoad := i18n.T(rctx, "hosts.kind.load")
	// Разделитель ", " окружён пробелами разметки шаблона (переносы строк
	// вокруг { i, ... } в hosts.templ) — сравниваем нестрого по пробелам.
	kindsRe := regexp.MustCompile(regexp.QuoteMeta(wantDisk) + `\s*,\s*` + regexp.QuoteMeta(wantLoad))
	if !kindsRe.MatchString(html) {
		t.Errorf("нет перечисления видов проблемного статуса через запятую: %s", html)
	}
	wantSilent := i18n.T(rctx, "hosts.status.silent")
	if !strings.Contains(html, wantSilent) {
		t.Errorf("нет статуса «тихий»: %s", html)
	}
	wantOK := i18n.T(rctx, "hosts.status.ok")
	if !strings.Contains(html, wantOK) {
		t.Errorf("нет статуса «норма»: %s", html)
	}
}

// TestHostsTableMetricsValues — добор покрытия hostsTable/hostPercentText/
// hostLoadText: строка с заполненными CPU/Mem/Disk/LoadPerCore (ветка «есть
// значение», не «нет данных»/прочерк, покрытый другими тестами).
func TestHostsTableMetricsValues(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	cpu, mem, disk, load := 0.421, 0.75, 0.9, 1.5
	rows := []templates.HostRowVM{
		{Name: "web-1", StatusKind: "ok", CPU: &cpu, Mem: &mem, Disk: &disk, LoadPerCore: &load},
	}
	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	for _, want := range []string{"42%", "75%", "90%", "1.50"} {
		if !strings.Contains(html, want) {
			t.Errorf("нет значения метрики %q: %s", want, html)
		}
	}
}

// Прямой вызов hostLabelText с непустым значением (ветка «есть значение» —
// в hostsTable функция вызывается только с пустой строкой, непустая метка
// рисуется бейджем в отдельной ветке шаблона) — см.
// internal/web/templates/hosts_helpers_test.go (тот же пакет, unexported).

// TestHostsFilterBarNewOnlyAndGroupRole — добор покрытия hostsFilterBar:
// чип «новые» активен (NewOnly=true, ветка aria-current), переключатель
// группировки на сегменте "role" (третье значение цикла, до этого
// покрывались только "none" и "env").
func TestHostsFilterBarNewOnlyAndGroupRole(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{{Name: "web-1", StatusKind: "ok"}}
	filter := templates.HostsFilterVM{NewOnly: true, Active: true, Group: "role"}
	facets := templates.NewHostsFacets(rctx, 1, filter, []string{"prod"}, []string{"web"})

	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, filter, facets, nil, "", "", "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	wantNew := i18n.T(rctx, "hosts.filter.new")
	if !strings.Contains(html, `aria-current="page">`+wantNew+`</a>`) {
		t.Errorf("чип «новые» не отмечен активным при NewOnly=true: %s", html)
	}
	wantRole := i18n.T(rctx, "hosts.group.role")
	if !strings.Contains(html, `aria-current="page">`+wantRole+`</a>`) {
		t.Errorf("сегмент «по роли» не отмечен активным при Group=role: %s", html)
	}
}

// TestHostsListCollectorConfigDetailsInstallCmd — добор покрытия
// hostsCollectorConfigDetails/HostsList: список НЕ пуст (не онбординг),
// installCmd непуст — раскрывающийся блок команды агента; truncated=true —
// подсказка усечения списка.
func TestHostsListCollectorConfigDetailsInstallCmd(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{{Name: "web-1", StatusKind: "ok"}}
	installCmd := agentInstallCommand("https://g.example", "pk_x")
	config := collectorConfig("https://g.example", "pk_x")

	var sb strings.Builder
	if err := templates.HostsList(1, rows, true, 1, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, installCmd, config, "", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "GOTCHA_AGENT_KEY=pk_x") {
		t.Errorf("нет блока команды установки агента в непустом списке: %s", html)
	}
	if !strings.Contains(html, "otlphttp") {
		t.Errorf("нет свёрнутого конфига коллектора в непустом списке: %s", html)
	}
	wantLimitNotice := i18n.Tf(rctx, "hosts.limit_notice", "limit", "1")
	if !strings.Contains(html, wantLimitNotice) {
		t.Errorf("нет подсказки усечения списка при truncated=true: %s", html)
	}
}

// TestHostsListCollectorConfigDetailsDist/Insecure — те же ветки
// hostsCollectorConfigDetails, что и в онбординге (rem-A sec-M1/sec-M4), но
// в контексте непустого списка хостов, где функция вызывается отдельно от
// hostsOnboarding.
func TestHostsListCollectorConfigDetailsDist(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{{Name: "web-1", StatusKind: "ok"}}
	config := collectorConfig("https://g.example", "pk_x")

	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, "", config, "dist", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	wantHint := i18n.T(rctx, "hosts.onboarding.agent_unavailable")
	if !strings.Contains(html, wantHint) {
		t.Errorf("нет подсказки о недоступной раздаче в непустом списке: %s", html)
	}
}

func TestHostsListCollectorConfigDetailsInsecure(t *testing.T) {
	rctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	rows := []templates.HostRowVM{{Name: "web-1", StatusKind: "ok"}}
	config := collectorConfig("http://gotcha.example", "pk_x")

	var sb strings.Builder
	if err := templates.HostsList(1, rows, false, hostsListLimit, templates.HostsFilterVM{}, templates.HostsFacets{}, nil, "", config, "insecure", "").Render(rctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	wantHint := i18n.T(rctx, "hosts.onboarding.agent_insecure")
	if !strings.Contains(html, wantHint) {
		t.Errorf("нет предупреждения про HTTPS в непустом списке: %s", html)
	}
}

// TestHostDetailEnvironmentRoleBadges — добор покрытия HostDetail: бейджи
// environment/role в шапке карточки (title=подпись колонки, тот же принцип,
// что у строки списка).
func TestHostDetailEnvironmentRoleBadges(t *testing.T) {
	vm := templates.HostDetailVM{
		Host: host.Host{Name: "web-1", Environment: "prod", Role: "db"},
	}
	html := renderHostDetail(t, vm)
	if !strings.Contains(html, `<span class="badge badge-neutral" title="Окружение">prod</span>`) {
		t.Errorf("нет бейджа environment в шапке карточки: %s", html)
	}
	if !strings.Contains(html, `<span class="badge badge-neutral" title="Роль">db</span>`) {
		t.Errorf("нет бейджа role в шапке карточки: %s", html)
	}
}

// TestHostDetailUptimeShown/NotShown — добор покрытия HostDetail: плитка
// «время работы» рисуется только при непустом Uptime (хост уже отчитался
// метрикой), а не «0».
func TestHostDetailUptimeShown(t *testing.T) {
	vm := templates.HostDetailVM{Host: host.Host{Name: "web-1"}, Uptime: "3д 4ч"}
	html := renderHostDetail(t, vm)
	if !strings.Contains(html, "3д 4ч") {
		t.Errorf("нет плитки uptime при непустом Uptime: %s", html)
	}
}

func TestHostDetailUptimeNotShown(t *testing.T) {
	vm := templates.HostDetailVM{Host: host.Host{Name: "web-1"}}
	html := renderHostDetail(t, vm)
	wantLabel := "Время работы"
	if strings.Contains(html, wantLabel) {
		t.Errorf("плитка uptime показана при пустом Uptime: %s", html)
	}
}

// TestHostDetailAgentUpdateBadgeWithoutCmd — AgentUpdateAvailable=true, но
// AgentUpdateCmd пуст (путь агента недоступен/небезопасен, rem-A sec-M1/
// sec-M4): бейдж «есть обновление» в плитке версии остаётся, но свёрнутый
// блок с готовой командой не рендерится (комбинация && во втором операнде).
func TestHostDetailAgentUpdateBadgeWithoutCmd(t *testing.T) {
	vm := templates.HostDetailVM{
		Host:                 host.Host{Name: "web-1", AgentVersion: "0.5.0"},
		AgentVersion:         "0.5.0",
		AgentUpdateAvailable: true,
		AgentUpdateCmd:       "",
	}
	html := renderHostDetail(t, vm)
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantBadge := i18n.T(ctx, "hosts.detail.agent_update")
	if !strings.Contains(html, wantBadge) {
		t.Errorf("нет бейджа обновления при AgentUpdateCmd=\"\": %s", html)
	}
	if strings.Contains(html, "<details class=\"host-agent-update\"") {
		t.Errorf("свёрнутый блок команды обновления показан при пустом AgentUpdateCmd: %s", html)
	}
}

// TestHostDetailOpenIncidents — добор покрытия HostDetail/hostOpenIncidentRow:
// список открытых инцидентов с двумя строками — Detail заполнен и Detail
// пуст (обе ветки docblock-примечания hostOpenIncidentRow).
func TestHostDetailOpenIncidents(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	vm := templates.HostDetailVM{
		Host: host.Host{Name: "web-1"},
		OpenIncidents: []host.Incident{
			{Kind: "disk", CurrentValue: 0.93, Detail: "/var", StartedAt: started},
			{Kind: "load", CurrentValue: 1.8, StartedAt: started},
		},
	}
	html := renderHostDetail(t, vm)

	if !strings.Contains(html, "93.0%") {
		t.Errorf("нет значения диска в строке открытого инцидента: %s", html)
	}
	if !strings.Contains(html, "(/var)") {
		t.Errorf("нет детали инцидента в скобках: %s", html)
	}
	if !strings.Contains(html, "1.80×") {
		t.Errorf("нет значения load в строке открытого инцидента: %s", html)
	}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantEmpty := i18n.T(ctx, "hosts.detail.incidents_open_empty")
	if strings.Contains(html, wantEmpty) {
		t.Errorf("подсказка «нет открытых» показана при непустом списке: %s", html)
	}
}

// TestHostDetailRecentIncidents — добор покрытия HostDetail/hostIncidentRow:
// таблица истории инцидентов с открытой и решённой строкой.
func TestHostDetailRecentIncidents(t *testing.T) {
	vm := templates.HostDetailVM{
		Host: host.Host{Name: "web-1"},
		RecentIncidents: []host.Incident{
			{Kind: "memory", Status: "open", PeakValue: 0.95, CurrentValue: 0.91},
			{Kind: "disk", Status: "resolved", PeakValue: 0.92, CurrentValue: 0.5},
		},
	}
	html := renderHostDetail(t, vm)

	if !strings.Contains(html, "95.0%") || !strings.Contains(html, "91.0%") {
		t.Errorf("нет пиковых/текущих значений истории инцидентов: %s", html)
	}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantOpen := i18n.T(ctx, "metrics.alerts.status.open")
	wantResolved := i18n.T(ctx, "metrics.alerts.status.resolved")
	if !strings.Contains(html, wantOpen) {
		t.Errorf("нет статуса «открыт» в истории: %s", html)
	}
	if !strings.Contains(html, wantResolved) {
		t.Errorf("нет статуса «решён» в истории: %s", html)
	}
	wantEmpty := i18n.T(ctx, "hosts.detail.incidents_empty")
	if strings.Contains(html, wantEmpty) {
		t.Errorf("подсказка «истории нет» показана при непустом списке: %s", html)
	}
}

// TestHostDetailCanOperate — добор покрытия HostDetail: форма удаления
// хоста рендерится только при CanOperate=true (operator+, остальные тесты
// пакета держат его в значении по умолчанию false).
func TestHostDetailCanOperate(t *testing.T) {
	vm := templates.HostDetailVM{Host: host.Host{Name: "web-1"}, CanOperate: true}
	html := renderHostDetail(t, vm)
	if !strings.Contains(html, `class="host-actions"`) {
		t.Errorf("нет формы удаления хоста при CanOperate=true: %s", html)
	}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantDelete := i18n.T(ctx, "hosts.detail.delete")
	if !strings.Contains(html, wantDelete) {
		t.Errorf("нет текста кнопки удаления: %s", html)
	}
}

// TestHostStatusTextKinds — добор покрытия hostStatusText: три ветки
// switch по kind в бейдже шапки карточки (problem с несколькими
// ProblemKinds — та же ветка разделителя ", ", что у hostsTable; silent;
// default ok), считаем и через HostDetail напрямую.
func TestHostStatusTextKinds(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	problem := renderHostDetail(t, templates.HostDetailVM{
		Host: host.Host{Name: "web-1"}, StatusKind: "problem", ProblemKinds: []string{"memory", "disk"},
	})
	wantMem := i18n.T(ctx, "hosts.kind.memory")
	wantDisk := i18n.T(ctx, "hosts.kind.disk")
	kindsRe := regexp.MustCompile(regexp.QuoteMeta(wantMem) + `\s*,\s*` + regexp.QuoteMeta(wantDisk))
	if !kindsRe.MatchString(problem) {
		t.Errorf("нет перечисления видов проблемного статуса в шапке карточки: %s", problem)
	}

	silent := renderHostDetail(t, templates.HostDetailVM{Host: host.Host{Name: "web-1"}, StatusKind: "silent"})
	wantSilent := i18n.T(ctx, "hosts.status.silent")
	if !strings.Contains(silent, wantSilent) {
		t.Errorf("нет статуса «тихий» в шапке карточки: %s", silent)
	}

	ok := renderHostDetail(t, templates.HostDetailVM{Host: host.Host{Name: "web-1"}, StatusKind: "ok"})
	wantOK := i18n.T(ctx, "hosts.status.ok")
	if !strings.Contains(ok, wantOK) {
		t.Errorf("нет статуса «норма» в шапке карточки: %s", ok)
	}
}

// TestHostChartCardBranches — добор покрытия hostChartCard: три состояния
// графика — Empty=true (пустое состояние со скрейпер-подсказкой), Empty=
// false без легенды/усечения, Empty=false с легендой и Truncated=true
// (подпись топ-N групп).
func TestHostChartCardBranches(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})

	empty := templates.HostDetailVM{
		Host:   host.Host{Name: "web-1"},
		Charts: []templates.HostChartVM{{Key: "cpu", Empty: true}},
	}
	htmlEmpty := renderHostDetail(t, empty)
	wantScraper := i18n.T(ctx, "hosts.scraper_hint.cpu")
	if !strings.Contains(htmlEmpty, wantScraper) {
		t.Errorf("нет подсказки скрейпера в пустом состоянии графика: %s", htmlEmpty)
	}

	plain := templates.HostDetailVM{
		Host: host.Host{Name: "web-1"},
		Charts: []templates.HostChartVM{
			{Key: "mem", Empty: false, Chart: templ.Raw("<svg data-test-chart=\"mem\"></svg>")},
		},
	}
	htmlPlain := renderHostDetail(t, plain)
	if !strings.Contains(htmlPlain, `data-test-chart="mem"`) {
		t.Errorf("нет отрисованного графика без легенды: %s", htmlPlain)
	}

	withLegend := templates.HostDetailVM{
		Host: host.Host{Name: "web-1"},
		Charts: []templates.HostChartVM{
			{
				Key:       "load",
				Empty:     false,
				Chart:     templ.Raw("<svg data-test-chart=\"load\"></svg>"),
				Legend:    []templates.LegendItem{{Label: "core-0", Class: "series-0"}},
				Truncated: true,
			},
		},
	}
	htmlLegend := renderHostDetail(t, withLegend)
	if !strings.Contains(htmlLegend, "core-0") {
		t.Errorf("нет легенды графика: %s", htmlLegend)
	}
	wantTruncated := i18n.Tf(ctx, "hosts.chart.top_groups", "n", "8")
	if metric.MaxSeriesGroups != 8 {
		t.Fatalf("metric.MaxSeriesGroups изменился (%d) — обнови ожидание подписи усечения", metric.MaxSeriesGroups)
	}
	if !strings.Contains(htmlLegend, wantTruncated) {
		t.Errorf("нет подписи усечения топ-N групп: %s", htmlLegend)
	}
}

// TestHostSettingsAgentInstallBlock — T14: страница настроек порогов несёт
// свёрнутый блок с готовой командой установки агента рядом со свёрнутым
// блоком конфига коллектора (hostsCollectorConfigDetails, дополненный этой
// задачей) — второй сервер подключают тем же путём, что и первый (§5.4).
func TestHostSettingsAgentInstallBlock(t *testing.T) {
	installCmd := agentInstallCommand("https://g.example", "pk_x")
	config := collectorConfig("https://g.example", "pk_x")
	html := renderHostSettingsPage(t, installCmd, config, "")

	for _, want := range []string{"GOTCHA_AGENT_ENDPOINT=https://g.example", "GOTCHA_AGENT_KEY=pk_x", "/install.sh"} {
		if !strings.Contains(html, want) {
			t.Errorf("страница настроек без фрагмента команды агента %q: %s", want, html)
		}
	}
	if !strings.Contains(html, "otlphttp") {
		t.Errorf("страница настроек без конфига коллектора: %s", html)
	}
}
