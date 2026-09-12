package guards

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// Ключ, забытый только в одном каталоге, ловится паритетом
// (internal/i18n/catalog_test.go), но забытый в обоих — только этим тестом.
func TestDynamicKeysResolve(t *testing.T) {
	tree := Load(t)

	groups := map[string][]string{
		"issues.status.":       issuesStatusValues(t, tree),
		"issues.level.":        issue.Levels,
		"probe.status.":        uptime.ProbeStatuses,
		"range.":               rangePresetKeys(),
		"org.quota.kind.":      quotaKindShortKeys(),
		"uptime.consensus.":    {string(uptime.ConsensusAny), string(uptime.ConsensusMajority), string(uptime.ConsensusAll)},
		"platform.":            org.Platforms,
		"uptime.kind.":         uptime.Kinds,
		"metrics.aggregation.": metric.Aggregations,
		"metrics.type.":        metric.MetricTypes,
		"hosts.kind.":          host.Kinds,
		"logs.severity.":       log.Severities,
		"recipes.":             recipeDynamicKeys(),
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

// Базовые ключи (org.quota.kind.events и т.д.) сюда не входят — собираются литералом,
// покрыты общим сканером каталога; здесь только реально конкатенируемая ".short".
func quotaKindShortKeys() []string {
	out := make([]string, 0, len(org.QuotaKinds))
	for _, k := range org.QuotaKinds {
		out = append(out, k+".short")
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
