package guards

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

const minFamilies = 31

type family struct {
	prefix   string
	values   []string
	suffixes []string // nil => ключ = prefix + value
}

func families(t *testing.T, tree *Tree) []family {
	t.Helper()
	return []family{
		{prefix: "issues.status.", values: issuesStatusValues(t, tree)},
		{prefix: "issues.level.", values: issue.Levels},
		{prefix: "probe.status.", values: uptime.ProbeStatuses},
		{prefix: "range.", values: rangePresetKeys()},
		{prefix: "org.quota.kind.", values: org.QuotaKinds, suffixes: []string{"", ".short"}},
		{prefix: "uptime.consensus.", values: []string{string(uptime.ConsensusAny), string(uptime.ConsensusMajority), string(uptime.ConsensusAll)}},
		{prefix: "platform.", values: org.Platforms},
		{prefix: "uptime.kind.", values: uptime.Kinds},
		{prefix: "metrics.aggregation.", values: metric.Aggregations},
		{prefix: "metrics.type.", values: metric.MetricTypes},
		{prefix: "hosts.kind.", values: host.Kinds},
		{prefix: "logs.severity.", values: log.Severities},
		{prefix: "recipes.", values: recipeDynamicKeys()},
		{prefix: "error.logfilter.", values: logfilterErrorCodes(t, tree)},
		{prefix: "host.threshold.scope.", values: checkInValues(t, migrationBody(t, tree, "0075_host_group_thresholds.up.sql"), "scope")},
		{prefix: "host.threshold.effective_from.", values: []string{string(host.LevelHost), string(host.LevelRole), string(host.LevelEnv), string(host.LevelProject), string(host.LevelDefault)}},
		{prefix: "project.settings.keys.kind.", values: []string{string(org.KindBrowser), string(org.KindServer), string(org.KindAgent), string(org.KindLegacy)}},
		{prefix: "perf.title.", values: []string{trace.KindNPlusOne, trace.KindSlowDBQuery, trace.KindHTTPFlood}},
		{prefix: "exports.status.", values: []string{string(export.StatusQueued), string(export.StatusRunning), string(export.StatusDone), string(export.StatusFailed), string(export.StatusExpired)}},
		{prefix: "exports.kind.", values: []string{string(export.KindIssues), string(export.KindEvents)}},
		{prefix: "exports.format.", values: []string{string(export.FormatCSV), string(export.FormatJSON), string(export.FormatNDJSON)}},
		// Источник — literal-результаты multiIf в internal/trace/query.go (AS kind,
		// строки ~225-227); значения меняют оба места разом, из Go их не перечислить.
		{prefix: "deps.kind.", values: []string{"database", "cache", "http"}},
		{prefix: "feed.source.", values: feedSourceValues(t, tree)},
		{prefix: "feed.group.root.", values: checkInValues(t, migrationBody(t, tree, "0079_incident_groups.up.sql"), "root_source")},
		{prefix: "hosts.group.", values: hostsGroupValues(t, tree)},
		{prefix: "hosts.chart.", values: hostChartKeys(t, tree)},
		{prefix: "hosts.scraper_hint.", values: hostChartKeys(t, tree)},
		{prefix: "notify.issue.kind.", values: []string{alert.KindNewIssue, alert.KindRegression, alert.KindSpike}},
		{prefix: "alerts.channels.kind.", values: []string{alert.ChannelEmail, alert.ChannelWebhook, alert.ChannelTelegram}},
		{prefix: "help.", values: helpAreasInTemplates(t, tree), suffixes: []string{".title", ".body"}},
		{prefix: "error.monitor.", values: monitorErrorCodes(t, tree)},
	}
}

func familyKeys(f family) []string {
	sfx := f.suffixes
	if len(sfx) == 0 {
		sfx = []string{""}
	}
	out := make([]string, 0, len(f.values)*len(sfx))
	for _, v := range f.values {
		for _, s := range sfx {
			out = append(out, f.prefix+v+s)
		}
	}
	return out
}

func familyKeySet(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range families(t, tree) {
		for _, k := range familyKeys(f) {
			out[k] = true
		}
	}
	return out
}

func familyPrefixes(fams []family) map[string]bool {
	out := map[string]bool{}
	for _, f := range fams {
		out[f.prefix] = true
	}
	return out
}

func familiesByPrefix(fams []family, prefix string) []family {
	var out []family
	for _, f := range fams {
		if f.prefix == prefix {
			out = append(out, f)
		}
	}
	return out
}

// Возвращает ВСЕ записи с самым длинным префиксом, подходящим ключу: семейства
// вложены (host.threshold. перекрывает host.threshold.scope.), и под одним
// префиксом может быть несколько записей с разными суффиксами.
func longestPrefixMatch(key string, fams []family) []family {
	var out []family
	longest := -1
	for _, f := range fams {
		if !strings.HasPrefix(key, f.prefix) {
			continue
		}
		switch {
		case len(f.prefix) > longest:
			longest = len(f.prefix)
			out = []family{f}
		case len(f.prefix) == longest:
			out = append(out, f)
		}
	}
	return out
}

func TestFamilyEntriesAreUnique(t *testing.T) {
	fams := families(t, Load(t))
	seen := map[string]bool{}
	for _, f := range fams {
		k := f.prefix + "\x00" + strings.Join(f.suffixes, ",")
		if seen[k] {
			t.Errorf("дубль записи семейства: префикс %q, суффиксы %v", f.prefix, f.suffixes)
		}
		seen[k] = true
	}
	if len(fams) < minFamilies {
		t.Fatalf("записей семейств %d, ожидалось не меньше %d: карта усохла", len(fams), minFamilies)
	}
}

func TestFamilyLongestPrefixWins(t *testing.T) {
	fams := families(t, Load(t))
	scope := familiesByPrefix(fams, "host.threshold.scope.")
	effectiveFrom := familiesByPrefix(fams, "host.threshold.effective_from.")
	if len(scope) == 0 || len(effectiveFrom) == 0 {
		t.Fatalf("не нашли записи host.threshold.scope. (%d) или host.threshold.effective_from. (%d)", len(scope), len(effectiveFrom))
	}
	// Короткий префикс host.threshold. здесь синтетический — реального источника
	// с таким префиксом в карте пока нет, но отбор по длине обязан работать и на нём.
	pool := append([]family{{prefix: "host.threshold.", values: []string{"warning"}}}, scope...)
	pool = append(pool, effectiveFrom...)

	got := longestPrefixMatch("host.threshold.scope.warning", pool)
	if len(got) != 1 || got[0].prefix != "host.threshold.scope." {
		t.Fatalf("отбор для host.threshold.scope.warning вернул %v, ожидали одну запись host.threshold.scope.", got)
	}

	got = longestPrefixMatch("host.threshold.effective_from.host", pool)
	if len(got) != 1 || got[0].prefix != "host.threshold.effective_from." {
		t.Fatalf("отбор для host.threshold.effective_from.host вернул %v, ожидали одну запись host.threshold.effective_from.", got)
	}
}
