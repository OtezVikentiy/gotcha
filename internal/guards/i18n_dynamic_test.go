package guards

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// Ключ, забытый только в одном каталоге, ловится паритетом
// (internal/i18n/catalog_test.go), но забытый в обоих — только этим тестом.
func TestDynamicKeysResolve(t *testing.T) {
	tree := Load(t)

	groups := map[string][]string{
		"issues.status.":                 issuesStatusValues(t, tree),
		"issues.level.":                  issue.Levels,
		"probe.status.":                  uptime.ProbeStatuses,
		"range.":                         rangePresetKeys(),
		"org.quota.kind.":                quotaKindShortKeys(),
		"uptime.consensus.":              {string(uptime.ConsensusAny), string(uptime.ConsensusMajority), string(uptime.ConsensusAll)},
		"platform.":                      org.Platforms,
		"uptime.kind.":                   uptime.Kinds,
		"metrics.aggregation.":           metric.Aggregations,
		"metrics.type.":                  metric.MetricTypes,
		"hosts.kind.":                    host.Kinds,
		"logs.severity.":                 log.Severities,
		"recipes.":                       recipeDynamicKeys(),
		"error.logfilter.":               logfilterErrorCodes(t, tree),
		"host.threshold.scope.":          checkInValues(t, migrationBody(t, tree, "0075_host_group_thresholds.up.sql"), "scope"),
		"host.threshold.effective_from.": {string(host.LevelHost), string(host.LevelRole), string(host.LevelEnv), string(host.LevelProject), string(host.LevelDefault)},
		"project.settings.keys.kind.":    {string(org.KindBrowser), string(org.KindServer), string(org.KindAgent), string(org.KindLegacy)},
		"perf.title.":                    {trace.KindNPlusOne, trace.KindSlowDBQuery, trace.KindHTTPFlood},
		"exports.status.":                {string(export.StatusQueued), string(export.StatusRunning), string(export.StatusDone), string(export.StatusFailed), string(export.StatusExpired)},
		"exports.kind.":                  {string(export.KindIssues), string(export.KindEvents)},
		"exports.format.":                {string(export.FormatCSV), string(export.FormatJSON), string(export.FormatNDJSON)},
		// Источник — literal-результаты multiIf в internal/trace/query.go (AS kind,
		// строки ~225-227); значения меняют оба места разом, из Go их не перечислить.
		"deps.kind.":            {"database", "cache", "http"},
		"feed.source.":          feedSourceValues(t, tree),
		"feed.group.root.":      checkInValues(t, migrationBody(t, tree, "0079_incident_groups.up.sql"), "root_source"),
		"hosts.group.":          hostsGroupValues(t, tree),
		"hosts.chart.":          hostChartKeys(t, tree),
		"hosts.scraper_hint.":   hostChartKeys(t, tree),
		"notify.issue.kind.":    {alert.KindNewIssue, alert.KindRegression, alert.KindSpike},
		"alerts.channels.kind.": {alert.ChannelEmail, alert.ChannelWebhook, alert.ChannelTelegram},
	}
	// Пустая группа — сигнал, что сборка самой группы сломана, а не что в
	// каталоге всё в порядке: пустой срез не даст ни одной находки.
	for prefix, values := range groups {
		if len(values) == 0 {
			t.Fatalf("группа %q пуста — сборка множества значений сломана, а не каталог", prefix)
		}
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for prefix, values := range groups {
			for _, v := range values {
				key := prefix + v
				if got := i18n.T(ctx, key); got == key {
					t.Errorf("[%s] ключ %q собирается в коде, но перевода нет — на странице будет сырой ключ", lang, key)
				}
			}
		}
	}
}

// "custom" сюда сознательно не входит: ключ "range.custom" вызывается
// литералом и уже покрыт общим сканером каталога (i18n_keys_test.go).
func rangePresetKeys() []string {
	out := make([]string, 0, len(web.TimeRangePresets)+1)
	out = append(out, web.RangeAll)
	for k := range web.TimeRangePresets {
		out = append(out, k)
	}
	return out
}

// Базовые ключи (org.quota.kind.events и т.д.) собираются и литералом (покрыто общим
// сканером каталога), и конкатенацией в quotaBanner — оба варианта здесь, наравне с ".short".
func quotaKindShortKeys() []string {
	out := make([]string, 0, len(org.QuotaKinds)*2)
	for _, k := range org.QuotaKinds {
		out = append(out, k, k+".short")
	}
	return out
}

// Статические ключи страниц ("recipes.list.title" и т.д.) сюда сознательно
// не входят: они зовутся литералами и уже покрыты общим сканером каталога.
func recipeDynamicKeys() []string {
	var out []string
	for _, r := range recipes.All() {
		out = append(out, r.ID+".title", r.ID+".desc")
		for _, c := range r.Charts {
			out = append(out, r.ID+".chart."+c.Key)
			for _, s := range c.Series {
				if s.LabelSuffix != "" {
					out = append(out, r.ID+".series."+s.LabelSuffix)
				}
			}
		}
		for _, rule := range r.Rules {
			out = append(out, r.ID+".rule."+rule.NoteKey)
		}
	}
	return out
}

// Источник истины — CHECK-ограничение миграции, а не internal/issue.validStatuses:
// именно CHECK не даёт значению в БД разъехаться со списком.
func issuesStatusValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	return checkInValues(t, migrationBody(t, tree, "0003_issues.up.sql"), "status")
}

func migrationBody(t *testing.T, tree *Tree, pathSuffix string) string {
	t.Helper()
	for _, f := range tree.MigrationsPG {
		if strings.HasSuffix(f.Path, pathSuffix) {
			return f.Body
		}
	}
	t.Fatalf("миграция с суффиксом пути %q не найдена в дереве", pathSuffix)
	return ""
}

// `\s` между именем колонки и IN — CHECK иногда переносится на следующую
// строку после DEFAULT, а `\s` (в отличие от `.`) матчит перевод строки.
func checkInValues(t *testing.T, body, column string) []string {
	t.Helper()
	re := regexp.MustCompile(`CHECK\s*\(\s*` + regexp.QuoteMeta(column) + `\s+IN\s*\(([^)]*)\)\)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("не нашли CHECK (%s IN (...)) в миграции", column)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return out
}

func TestHelpPanelKeysResolve(t *testing.T) {
	tree := Load(t)
	areas := helpAreasInTemplates(t, tree)
	if len(areas) < 10 {
		t.Fatalf("найдено %d областей помощи — сканер сломан", len(areas))
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, area := range areas {
			for _, suffix := range []string{".title", ".body"} {
				key := "help." + area + suffix
				if got := i18n.T(ctx, key); got == key {
					t.Errorf("[%s] панель помощи раздела %q без ключа %q", lang, area, key)
				}
			}
		}
	}
}

func helpAreasInTemplates(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	const marker = `helpPanel("`
	for _, f := range tree.Templates {
		data := f.Body
		for i := 0; ; {
			j := strings.Index(data[i:], marker)
			if j < 0 {
				break
			}
			start := i + j + len(marker)
			end := strings.Index(data[start:], `"`)
			if end < 0 {
				break
			}
			seen[data[start:start+end]] = true
			i = start + end
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	return out
}

func TestMonitorErrorCodesResolve(t *testing.T) {
	tree := Load(t)
	codes := monitorErrorCodes(t, tree)
	if len(codes) < 20 {
		t.Fatalf("найдено %d кодов — сканер сломан", len(codes))
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, code := range codes {
			key := "error.monitor." + code
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] код валидации %q без сообщения (%s)", lang, code, key)
			}
		}
	}
}

// Ловит ключ, отсутствующий в ОБОИХ каталогах; забытый только в en.json не поймает —
// i18n.lookup фолбэчит на ru, это ловит TestCatalogsHaveIdenticalKeys.
func TestExportFailureReasonKeysResolve(t *testing.T) {
	keys := export.FailureReasonKeys
	// Пустой список — сигнал, что сборка среза в worker.go сломана, а не что
	// причин отказа не бывает.
	if len(keys) == 0 {
		t.Fatal("export.FailureReasonKeys пуст — сборка списка в worker.go сломана, а не множество причин опустело по замыслу")
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, key := range keys {
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] причина отказа выгрузки %q без перевода — на странице «Выгрузки» и в письме автору будет сырой ключ", lang, key)
			}
		}
	}
}

// Второй аргумент вызова invalid("field", "code", ...) — искомый код.
var invalidCallRe = regexp.MustCompile(`invalid\("[^"]*",\s*"([^"]+)"`)

func monitorErrorCodes(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Generated || !strings.HasPrefix(f.Path, "internal/uptime/") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for _, m := range invalidCallRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}

var logfilterValidationCodeRe = regexp.MustCompile(`ValidationError\{Code:\s*"([^"]+)"`)

func logfilterErrorCodes(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Generated || !strings.HasPrefix(f.Path, "internal/logfilter/") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for _, m := range logfilterValidationCodeRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}

// Ветки UNION ALL в group.go записаны двояко: с колонкой group_id перед
// литералом и без неё, литералом сразу после SELECT — берём обе формы.
var feedSourceLiteralRe = regexp.MustCompile(`(?:\w+\.group_id,\s*|SELECT\s+)'(\w+)'(?:::text)?,\s*\w+\.id,`)

func feedSourceValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Path != "internal/incidentgroup/group.go" {
			continue
		}
		for _, m := range feedSourceLiteralRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	return out
}

// Источник — литеральный срез фильтра группировки хостов в hosts.templ:
// `for _, g := range []string{"none", "env", "role"}`.
var hostsGroupLiteralRe = regexp.MustCompile(`range \[\]string\{([^}]*)\}`)
var quotedLiteralRe = regexp.MustCompile(`"([^"]*)"`)

func hostsGroupValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	for _, f := range tree.Templates {
		if f.Path != "internal/web/templates/hosts.templ" {
			continue
		}
		m := hostsGroupLiteralRe.FindStringSubmatch(f.Body)
		if m == nil {
			t.Fatalf("hosts.templ: не нашли range []string{...} с группами хостов — разметка изменилась")
		}
		var out []string
		for _, q := range quotedLiteralRe.FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
		return out
	}
	t.Fatalf("не нашли internal/web/templates/hosts.templ в дереве")
	return nil
}

// Источник — internal/web/hostcharts.go: литеральные HostChartVM{Key: "..."}
// плюс ключи, передаваемые в общий hostRateChart(..., key string, ...).
var hostChartKeyLiteralRe = regexp.MustCompile(`Key:\s*"(\w+)"`)
var hostRateChartCallRe = regexp.MustCompile(`hostRateChart\([^)]*?,\s*"(\w+)"\s*,`)

func hostChartKeys(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Path != "internal/web/hostcharts.go" {
			continue
		}
		for _, m := range hostChartKeyLiteralRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
		for _, m := range hostRateChartCallRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatalf("не нашли ни одного ключа графика хоста в hostcharts.go — разбор сломан")
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
