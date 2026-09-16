package guards

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// Ключ, забытый только в одном каталоге, ловится паритетом
// (internal/i18n/catalog_test.go), но забытый в обоих — только этим тестом.
func TestDynamicKeysResolve(t *testing.T) {
	tree := Load(t)
	fams := families(t, tree)

	// Пустая группа — сигнал, что сборка самой группы сломана, а не что в
	// каталоге всё в порядке: пустой срез не даст ни одной находки.
	for _, f := range fams {
		if len(f.values) == 0 {
			t.Fatalf("группа %q пуста — сборка множества значений сломана, а не каталог", f.prefix)
		}
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, f := range fams {
			for _, key := range familyKeys(f) {
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

// Ниже прежнего значения — сканер ослеп на одном из маркеров вызова, а не
// областей помощи стало меньше в шаблонах.
const minHelpAreas = 28

func TestHelpPanelKeysResolve(t *testing.T) {
	tree := Load(t)
	fams := familiesByPrefix(families(t, tree), "help.")
	if len(fams) != 1 {
		t.Fatalf("ожидали ровно одну запись help. в карте, нашли %d", len(fams))
	}
	areas := fams[0].values
	if len(areas) < minHelpAreas {
		t.Fatalf("сканер ослеп: найдено %d областей помощи, ожидалось не меньше %d", len(areas), minHelpAreas)
	}
}

func helpAreasInTemplates(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	// Область, переданная переменной вместо строкового литерала, сканеру не
	// видна — это ограничивает minHelpAreas, а не повод городить парсер.
	markers := []string{`helpPanel("`, `helpPanelWith("`}
	for _, f := range tree.Templates {
		data := f.Body
		for _, marker := range markers {
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
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	return out
}

func TestMonitorErrorCodesResolve(t *testing.T) {
	tree := Load(t)
	fams := familiesByPrefix(families(t, tree), "error.monitor.")
	if len(fams) != 1 {
		t.Fatalf("ожидали ровно одну запись error.monitor. в карте, нашли %d", len(fams))
	}
	codes := fams[0].values
	if len(codes) < 20 {
		t.Fatalf("найдено %d кодов — сканер сломан", len(codes))
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

// "none" — literal-ветка default в depDirectionKey, не входит в case и
// добавляется вручную.
var depDirectionCaseRe = regexp.MustCompile(`func depDirectionKey\(dir string\) string \{\s*switch dir \{\s*case\s+([^:]+):`)

func depsDirectionValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	for _, f := range tree.Templates {
		if f.Path != "internal/web/templates/dependencies.templ" {
			continue
		}
		m := depDirectionCaseRe.FindStringSubmatch(f.Body)
		if m == nil {
			t.Fatalf("dependencies.templ: не нашли case-перечисление в depDirectionKey — разметка изменилась")
		}
		var out []string
		for _, q := range quotedLiteralRe.FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
		if len(out) < 3 {
			t.Fatalf("нашли %d значений case в depDirectionKey, ожидалось не меньше 3 — сканер ослеп", len(out))
		}
		return append(out, "none")
	}
	t.Fatalf("не нашли internal/web/templates/dependencies.templ в дереве")
	return nil
}

// oauth.Registry заполняется условно по фичефлагам в cmd/gotcha/oauth.go и
// тесту недоступен — источник истины: метод Name() string, обязательный интерфейсом.
var oauthNameRe = regexp.MustCompile(`(?s)func \([^)]*\)\s+Name\(\)\s+string\s*\{\s*return\s+"([^"]+)"`)

const maxOAuthProviderExemptions = 1

// providerLabel (internal/web/auth.go) при промахе перевода отдаёт DisplayName() —
// для OIDC это кастомное имя администратора; перевод навсегда перекрыл бы его.
var oauthProviderExemptions = []Exemption{
	{Value: "oidc", Finding: "oauth.provider.oidc без перевода", Why: "OIDC — конфигурируемый провайдер, DisplayName() отдаёт кастомное имя администратора вместо бренда; перевод перекрыл бы его"},
}

func oauthProviderValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Generated || !strings.HasPrefix(f.Path, "internal/oauth/") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for _, m := range oauthNameRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) < 3 {
		t.Fatalf("нашли %d провайдеров oauth, ожидалось не меньше 3 — сканер ослеп", len(seen))
	}
	CheckExemptions(t, "oauth-provider-i18n", oauthProviderExemptions, maxOAuthProviderExemptions, seen)
	exempted := ExemptedValues(oauthProviderExemptions)
	out := make([]string, 0, len(seen))
	for p := range seen {
		if exempted[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Третье семейство порогов хоста помимо .scope. и .effective_from.: enum'а нет,
// значения — литералы hostsettings.templ (аргументы groupKindPart и вызов для "silent").
var hostThresholdKindArgRe = regexp.MustCompile(`groupKindPart\(ctx,\s*"(\w+)"`)
var hostThresholdSilentRe = regexp.MustCompile(`i18n\.T\(ctx,\s*"host\.threshold\.silent"\)`)

func hostThresholdKinds(t *testing.T, tree *Tree) []string {
	t.Helper()
	for _, f := range tree.Templates {
		if f.Path != "internal/web/templates/hostsettings.templ" {
			continue
		}
		seen := map[string]bool{}
		for _, m := range hostThresholdKindArgRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
		if hostThresholdSilentRe.MatchString(f.Body) {
			seen["silent"] = true
		}
		out := make([]string, 0, len(seen))
		for k := range seen {
			out = append(out, k)
		}
		if len(out) < 4 {
			t.Fatalf("нашли %d видов порога хоста в hostsettings.templ, ожидалось не меньше 4", len(out))
		}
		return out
	}
	t.Fatalf("не нашли internal/web/templates/hostsettings.templ в дереве")
	return nil
}
