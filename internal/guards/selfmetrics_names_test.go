package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wantSelfMetricNames — ПОЛНЫЙ пиннённый список имён self-метрик продукта
// (J1). До этого теста список нигде не был зафиксирован: новое имя,
// зарегистрированное где угодно (cmd/gotcha или internal), попадало в вывод
// /metrics и в дашборды операторов без единой точки, которая заметила бы
// его появление или исчезновение. Список отсортирован для читаемого diff'а
// при правке.
//
// Обе стороны сверки обязательны: имя в коде, которого нет здесь, — новая
// метрика, проскочившая мимо ревью контракта (мутация №2 T6); имя здесь,
// которого нет в коде, — мёртвая запись, которая годами убеждала бы
// читателя, что метрика существует (тот же класс дефекта, что и
// unitlessCounters в env_example_test.go).
var wantSelfMetricNames = []string{
	"gotcha_build_info",
	"gotcha_cardinality_collapsed_total",
	"gotcha_cardinality_tracked_values",
	"gotcha_entities_purged_total",
	"gotcha_escalation_scheduler_last_tick_timestamp_seconds",
	"gotcha_escalation_scheduler_tick_duration_seconds",
	"gotcha_export_queue_depth",
	"gotcha_export_queue_failed",
	"gotcha_export_queue_oldest_seconds",
	"gotcha_host_evaluator_last_tick_timestamp_seconds",
	"gotcha_host_evaluator_tick_duration_seconds",
	"gotcha_host_registration_failures_total",
	"gotcha_host_registrations_rejected_total",
	"gotcha_host_registrations_scope_skipped_total",
	"gotcha_i18n_missing_key_total",
	"gotcha_ingest_deprecated_path_total",
	"gotcha_ingest_key_rejections_total",
	"gotcha_ingest_rejected_total",
	"gotcha_memory_limit_bytes",
	"gotcha_metric_evaluator_last_tick_timestamp_seconds",
	"gotcha_metric_evaluator_tick_duration_seconds",
	"gotcha_notify_failed_total",
	"gotcha_notify_queue_depth",
	"gotcha_notify_queue_failed",
	"gotcha_notify_queue_oldest_seconds",
	"gotcha_notify_retried_total",
	"gotcha_notify_sent_total",
	"gotcha_pipeline_dropped_tasks_total",
	"gotcha_pipeline_queue_bytes",
	"gotcha_pipeline_queue_capacity",
	"gotcha_pipeline_queue_depth",
	"gotcha_profile_evaluator_last_tick_timestamp_seconds",
	"gotcha_profile_evaluator_tick_duration_seconds",
	"gotcha_projects_purged_total",
	"gotcha_purge_queue_depth",
	"gotcha_purge_queue_oldest_seconds",
	"gotcha_slo_evaluator_last_tick_timestamp_seconds",
	"gotcha_slo_evaluator_tick_duration_seconds",
	"gotcha_storage_free_bytes",
	"gotcha_storage_total_bytes",
	"gotcha_storage_used_bytes",
	"gotcha_trace_evaluator_last_tick_timestamp_seconds",
	"gotcha_trace_evaluator_tick_duration_seconds",
	"gotcha_uptime_heartbeat_ignored_total",
	"gotcha_uptime_runner_last_tick_timestamp_seconds",
	"gotcha_uptime_runner_tick_duration_seconds",
	"gotcha_uptime_scheduler_last_tick_timestamp_seconds",
	"gotcha_uptime_scheduler_tick_duration_seconds",
	"gotcha_uptime_watchdog_last_tick_timestamp_seconds",
	"gotcha_uptime_watchdog_tick_duration_seconds",
	"gotcha_web_cross_origin_rejected_total",
	"gotcha_writer_buffered_rows",
	"gotcha_writer_dropped_rows_total",
	"gotcha_writer_insert_failures_total",
}

// wantQueueCanonNames — подмножество wantSelfMetricNames, которое обязано
// существовать под именем канона очереди (J1): gotcha_<подсистема>_queue_depth
// / _queue_oldest_seconds / _queue_failed / _queue_capacity / _queue_bytes.
// export и notify переименованы под покойный canon у purge; pipeline
// переименован частично (queued_tasks→queue_depth, queued_bytes→queue_bytes),
// queue_capacity у него уже был в каноне. Это ПОЗИТИВНАЯ проверка: она ловит
// ровно ту мутацию, которую просит T6 ("gotcha_export_queue_depth" →
// "gotcha_export_depth" — переименование МИМО канона теряет сегмент
// "_queue_", и никакой поиск по живому списку такое отсутствие не найдёт,
// если не сверять с тем, что ДОЛЖНО быть).
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

// bareQueueSuffix — ловит имя вида gotcha_<однословная подсистема>_depth (или
// _oldest_seconds/_failed/_capacity/_bytes) БЕЗ сегмента "_queue_" между
// подсистемой и словом канона. Ровно эта форма получается, если канонiческое
// имя переименовать мимо канона, срезав слово "queue" (пример из брифа T6:
// "gotcha_export_queue_depth" → "gotcha_export_depth"). Проверено, что она НЕ
// цепляет ни одно из текущих 52 пиннённых имён (gotcha_purge_queue_depth не
// совпадает — между "purge" и "depth" стоит "_queue_", а не пусто;
// gotcha_memory_limit_bytes не совпадает — "memory_limit" не однословно, а
// [a-z]+ подсистемы не захватывает "_").
var bareQueueSuffix = regexp.MustCompile(`^gotcha_[a-z]+_(depth|oldest_seconds|failed|capacity|bytes)$`)

// collectSelfMetricNames — имена self-метрик (первый строковый литерал в
// пятиаргументном selfmetrics.Add/AddInt), реально зарегистрированные в
// дереве. Та же AST-детекция, что у TestSelfMetricsDocumented
// (selfmetrics_docs_test.go) — переиспользовать буквально нельзя, скан там
// приватен и заточен под сбор списка файлов регистрации, здесь нужен только
// плоский набор имён.
func collectSelfMetricNames(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, gf := range tree.GoFiles {
		if gf.Generated || strings.HasSuffix(gf.Path, "_test.go") ||
			strings.HasPrefix(gf.Path, "internal/guards/") {
			continue
		}
		if !strings.Contains(gf.Body, "selfmetrics.") {
			continue
		}
		f, err := parser.ParseFile(fset, gf.Path, gf.Body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.Path, err)
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
			typ, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || pkg.Name != "selfmetrics" {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true // строковость литерала уже проверяет TestSelfMetricsDocumented
			}
			names[strings.Trim(lit.Value, `"`)] = true
			return true
		})
	}
	return names
}

// TestSelfMetricNamesPinned — сверяет РЕАЛЬНО зарегистрированные имена
// self-метрик с wantSelfMetricNames в обе стороны.
func TestSelfMetricNamesPinned(t *testing.T) {
	tree := Load(t)
	live := collectSelfMetricNames(t, tree)
	if len(live) < 10 {
		t.Fatalf("collected only %d self-metric names — the scanner is broken", len(live))
	}

	want := map[string]bool{}
	for _, n := range wantSelfMetricNames {
		if want[n] {
			t.Errorf("wantSelfMetricNames has a duplicate entry: %s", n)
		}
		want[n] = true
	}
	if sorted := append([]string(nil), wantSelfMetricNames...); !sort.StringsAreSorted(sorted) {
		t.Errorf("wantSelfMetricNames is not sorted — keep it diffable")
	}

	for n := range live {
		if !want[n] {
			t.Errorf("self-metric %q is registered in code but missing from wantSelfMetricNames in this test — pin it (or is this an accidental new metric?)", n)
		}
	}
	for n := range want {
		if !live[n] {
			t.Errorf("wantSelfMetricNames pins %q, but no code registers it anymore — stale entry, remove it", n)
		}
	}
}

// TestSelfMetricQueueNamingCanon — J1: любая метрика "глубина/возраст
// старейшего/отказы/ёмкость/байты" очереди обязана называться
// gotcha_<подсистема>_queue_<канон>, форма, в которой уже был purge.
func TestSelfMetricQueueNamingCanon(t *testing.T) {
	tree := Load(t)
	live := collectSelfMetricNames(t, tree)

	// Позитив: канонические имена очередей действительно существуют. Ловит
	// переименование МИМО канона (сегмент "_queue_" пропал) — общий пример
	// из брифа T6.
	for _, want := range wantQueueCanonNames {
		if !live[want] {
			t.Errorf("canonical queue metric %q is not registered — renamed away from canon?", want)
		}
	}

	// Негатив: старый, дореформенный словарь очередей нигде не должен
	// возвращаться (кто-то отменит переименование одной метрики, оставив
	// остальные в новом виде).
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
