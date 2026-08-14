package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
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
