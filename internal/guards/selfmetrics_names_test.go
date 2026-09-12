package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const selfMetricsImportPath = "gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"

type selfMetricSpec struct {
	name string
	typ  string // "Counter" или "Gauge"
}

// список отсортирован по имени для читаемого diff'а при правке.
var wantSelfMetrics = []selfMetricSpec{
	{"gotcha_build_info", "Gauge"},
	{"gotcha_cardinality_collapsed_total", "Counter"},
	{"gotcha_cardinality_tracked_values", "Gauge"},
	{"gotcha_entities_purged_total", "Counter"},
	{"gotcha_escalation_scheduler_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_escalation_scheduler_tick_duration_seconds", "Gauge"},
	{"gotcha_export_janitor_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_export_janitor_tick_duration_seconds", "Gauge"},
	{"gotcha_export_queue_depth", "Gauge"},
	{"gotcha_export_queue_failed", "Gauge"},
	{"gotcha_export_queue_oldest_seconds", "Gauge"},
	{"gotcha_host_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_host_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_host_registration_failures_total", "Counter"},
	{"gotcha_host_registrations_rejected_total", "Counter"},
	{"gotcha_host_registrations_scope_skipped_total", "Counter"},
	{"gotcha_i18n_missing_key_total", "Counter"},
	{"gotcha_ingest_deprecated_path_total", "Counter"},
	{"gotcha_ingest_key_rejections_total", "Counter"},
	{"gotcha_ingest_rejected_total", "Counter"},
	{"gotcha_memory_limit_bytes", "Gauge"},
	{"gotcha_metric_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_metric_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_metric_points_clock_skew_total", "Counter"},
	{"gotcha_notify_failed_total", "Counter"},
	{"gotcha_notify_queue_depth", "Gauge"},
	{"gotcha_notify_queue_failed", "Gauge"},
	{"gotcha_notify_queue_oldest_seconds", "Gauge"},
	{"gotcha_notify_retried_total", "Counter"},
	{"gotcha_notify_sent_total", "Counter"},
	{"gotcha_pipeline_backpressure_wait_seconds_total", "Counter"},
	{"gotcha_pipeline_backpressure_waits_total", "Counter"},
	{"gotcha_pipeline_dropped_tasks_total", "Counter"},
	{"gotcha_pipeline_queue_bytes", "Gauge"},
	{"gotcha_pipeline_queue_capacity", "Gauge"},
	{"gotcha_pipeline_queue_depth", "Gauge"},
	{"gotcha_profile_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_profile_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_projects_purged_total", "Counter"},
	{"gotcha_purge_queue_depth", "Gauge"},
	{"gotcha_purge_queue_oldest_seconds", "Gauge"},
	{"gotcha_secret_key_insecure", "Gauge"},
	{"gotcha_slo_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_slo_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_storage_free_bytes", "Gauge"},
	{"gotcha_storage_total_bytes", "Gauge"},
	{"gotcha_storage_used_bytes", "Gauge"},
	{"gotcha_trace_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_trace_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_uptime_heartbeat_ignored_total", "Counter"},
	{"gotcha_uptime_runner_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_uptime_runner_tick_duration_seconds", "Gauge"},
	{"gotcha_uptime_scheduler_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_uptime_scheduler_tick_duration_seconds", "Gauge"},
	{"gotcha_uptime_watchdog_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_uptime_watchdog_tick_duration_seconds", "Gauge"},
	{"gotcha_web_cross_origin_rejected_total", "Counter"},
	{"gotcha_writer_buffered_rows", "Gauge"},
	{"gotcha_writer_dropped_rows_total", "Counter"},
	{"gotcha_writer_insert_failures_total", "Counter"},
}

var wantQueueCanonNames = []string{
	"gotcha_export_queue_depth",
	"gotcha_export_queue_oldest_seconds",
	"gotcha_export_queue_failed",
	"gotcha_notify_queue_depth",
	"gotcha_notify_queue_oldest_seconds",
	"gotcha_notify_queue_failed",
	"gotcha_pipeline_queue_depth",
	"gotcha_pipeline_queue_bytes",
	"gotcha_pipeline_queue_capacity",
	"gotcha_purge_queue_depth",
	"gotcha_purge_queue_oldest_seconds",
}

// ловит имя без сегмента "_queue_" между однословной подсистемой и словом
// канона — форма, которая получается при переименовании мимо канона.
var bareQueueSuffix = regexp.MustCompile(`^gotcha_[a-z]+_(depth|oldest_seconds|failed|capacity|bytes)$`)

func nonLiteralSelfMetricTypeMsg(localPkgName string) string {
	if localPkgName == "." {
		return "self-metric type is not a literal package selector — the file dot-imports selfmetrics, which turns the type into an unqualified identifier and makes it unrecognizable as a frozen literal"
	}
	return fmt.Sprintf("self-metric type is not a literal %s.<Type> selector — registering through a wrapper or a variable takes the metric out from under the name/type freeze", localPkgName)
}

type selfMetricInventory struct {
	types     map[string]string   // имя → тип (selfmetrics.Counter/Gauge)
	callSites int                 // сырое число найденных call-site'ов — "сторож ослеп"
	files     map[string][]string // имя → файлы, где оно зарегистрировано
}

func collectSelfMetrics(t *testing.T, tree *Tree) selfMetricInventory {
	t.Helper()
	fset := token.NewFileSet()
	types := map[string]string{}
	files := map[string][]string{}
	callSites := 0
	for _, gf := range tree.GoFiles {
		if gf.Generated || strings.HasSuffix(gf.Path, "_test.go") ||
			strings.HasPrefix(gf.Path, "internal/guards/") {
			continue
		}
		if !strings.Contains(gf.Body, `internal/selfmetrics"`) {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
		}
		importsSelfMetrics := false
		localPkgName := "selfmetrics"
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != selfMetricsImportPath {
				continue
			}
			importsSelfMetrics = true
			if imp.Name != nil {
				localPkgName = imp.Name.Name
			}
			break
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 5 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Add" && sel.Sel.Name != "AddInt") {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			typSel, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				if importsSelfMetrics {
					t.Errorf("%s: %s", pos, nonLiteralSelfMetricTypeMsg(localPkgName))
				}
				return true
			}
			pkg, ok := typSel.X.(*ast.Ident)
			if !ok || pkg.Name != localPkgName {
				if importsSelfMetrics {
					t.Errorf("%s: %s", pos, nonLiteralSelfMetricTypeMsg(localPkgName))
				}
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: self-metric name is not a string literal — the contract can only freeze a name known at compile time", pos)
				return true
			}
			callSites++
			name := strings.Trim(lit.Value, `"`)
			typ := typSel.Sel.Name
			files[name] = append(files[name], gf.Path)
			if prev, seen := types[name]; seen && prev != typ {
				t.Errorf("%s: self-metric %q is registered as selfmetrics.%s here, but as selfmetrics.%s elsewhere — a metric cannot change type between registrations", pos, name, typ, prev)
				return true
			}
			types[name] = typ
			return true
		})
	}
	return selfMetricInventory{types: types, callSites: callSites, files: files}
}

func TestSelfMetricNamesPinned(t *testing.T) {
	tree := Load(t)
	scan := collectSelfMetrics(t, tree)
	live := scan.types
	if scan.callSites == 0 {
		t.Fatalf("blind guard: found 0 selfmetrics.Add/AddInt call-sites — the scan is looking at the wrong tree")
	}
	if scan.callSites < len(wantSelfMetrics) {
		t.Fatalf("blind guard: found only %d call-sites, fewer than the %d pinned metrics — the scanner is broken",
			scan.callSites, len(wantSelfMetrics))
	}
	if len(live) < 10 {
		t.Fatalf("collected only %d self-metric names — the scanner is broken", len(live))
	}

	want := map[string]string{}
	names := make([]string, 0, len(wantSelfMetrics))
	for _, s := range wantSelfMetrics {
		if _, dup := want[s.name]; dup {
			t.Errorf("wantSelfMetrics has a duplicate entry: %s", s.name)
		}
		want[s.name] = s.typ
		names = append(names, s.name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("wantSelfMetrics is not sorted by name — keep it diffable")
	}

	for n, typ := range live {
		wantTyp, ok := want[n]
		if !ok {
			t.Errorf("self-metric %q (selfmetrics.%s) is registered in code but missing from wantSelfMetrics in this test — pin it (or is this an accidental new metric?)", n, typ)
			continue
		}
		if typ != wantTyp {
			t.Errorf("self-metric %q changed type: code registers selfmetrics.%s, wantSelfMetrics pins selfmetrics.%s — update the golden entry only if the type change is intentional", n, typ, wantTyp)
		}
	}
	for n, typ := range want {
		if _, ok := live[n]; !ok {
			t.Errorf("wantSelfMetrics pins %q (selfmetrics.%s), but no code registers it anymore — stale entry, remove it", n, typ)
		}
	}
}

func TestSelfMetricQueueNamingCanon(t *testing.T) {
	tree := Load(t)
	live := collectSelfMetrics(t, tree).types

	for _, want := range wantQueueCanonNames {
		if _, ok := live[want]; !ok {
			t.Errorf("canonical queue metric %q is not registered — renamed away from canon?", want)
		}
	}

	bannedSubstrings := []string{
		"_pending_jobs", "_oldest_pending_age_seconds", "_failed_jobs",
		"_queued_tasks", "_queued_bytes",
	}
	for n := range live {
		for _, banned := range bannedSubstrings {
			if strings.Contains(n, banned) {
				t.Errorf("self-metric %q uses the pre-canon queue vocabulary (%q) — rename to the gotcha_<subsystem>_queue_* form", n, banned)
			}
		}
		if m := bareQueueSuffix.FindStringSubmatch(n); m != nil {
			t.Errorf("self-metric %q looks like a queue metric missing the \"_queue_\" segment — canon is gotcha_<subsystem>_queue_%s, not gotcha_<subsystem>_%s",
				n, m[1], m[1])
		}
	}
}
