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

// selfMetricsImportPath — путь пакета, чьё присутствие в импортах файла
// включает проверку «первый аргумент Add/AddInt обязан быть литеральным
// selfmetrics.<Type>» (см. importsSelfMetrics в collectSelfMetrics).
const selfMetricsImportPath = "gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"

// selfMetricSpec — одна пара (имя, тип) золотого списка self-метрик:
// строка имени и идентификатор selfmetrics.Counter/Gauge, которым метрика
// заведена в коде.
type selfMetricSpec struct {
	name string
	typ  string // "Counter" или "Gauge" — имя идентификатора пакета selfmetrics
}

// wantSelfMetrics — ПОЛНЫЙ пиннённый список self-метрик продукта (J1,
// расширен заморозкой контракта E3 типом метрики). До теста на одни имена
// список нигде не был зафиксирован: новое имя, зарегистрированное где
// угодно (cmd/gotcha или internal), попадало в вывод /metrics и в дашборды
// операторов без единой точки, которая заметила бы его появление или
// исчезновение. Тип пиннится тем же списком, а не отдельной копией: смена
// counter↔gauge меняет семантику панелей и алертов Grafana ровно так же
// незаметно, как переименование, — а два параллельных списка (имена
// отдельно, типы отдельно) завели бы ровно ту дыру, которую заморозка
// контракта закрывает, — сверку одной копии с другой вместо сверки с кодом.
// Список отсортирован по имени для читаемого diff'а при правке.
//
// Обе стороны сверки обязательны: имя в коде, которого нет здесь, — новая
// метрика, проскочившая мимо ревью контракта (мутация №2 T6); имя здесь,
// которого нет в коде, — мёртвая запись, которая годами убеждала бы
// читателя, что метрика существует (тот же класс дефекта, что и
// unitlessCounters в env_example_test.go). Несовпадение типа при
// совпадающем имени — отдельная третья мутация: переименования нет, но
// оператор получает gauge там, где Grafana ждёт монотонный counter (или
// наоборот), и ни одна из двух проверок на имя такое не поймает.
var wantSelfMetrics = []selfMetricSpec{
	{"gotcha_build_info", "Gauge"},
	{"gotcha_cardinality_collapsed_total", "Counter"},
	{"gotcha_cardinality_tracked_values", "Gauge"},
	{"gotcha_entities_purged_total", "Counter"},
	{"gotcha_escalation_scheduler_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_escalation_scheduler_tick_duration_seconds", "Gauge"},
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
	{"gotcha_notify_failed_total", "Counter"},
	{"gotcha_notify_queue_depth", "Gauge"},
	{"gotcha_notify_queue_failed", "Gauge"},
	{"gotcha_notify_queue_oldest_seconds", "Gauge"},
	{"gotcha_notify_retried_total", "Counter"},
	{"gotcha_notify_sent_total", "Counter"},
	{"gotcha_pipeline_dropped_tasks_total", "Counter"},
	{"gotcha_pipeline_queue_bytes", "Gauge"},
	{"gotcha_pipeline_queue_capacity", "Gauge"},
	{"gotcha_pipeline_queue_depth", "Gauge"},
	{"gotcha_profile_evaluator_last_tick_timestamp_seconds", "Gauge"},
	{"gotcha_profile_evaluator_tick_duration_seconds", "Gauge"},
	{"gotcha_projects_purged_total", "Counter"},
	{"gotcha_purge_queue_depth", "Gauge"},
	{"gotcha_purge_queue_oldest_seconds", "Gauge"},
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

// wantQueueCanonNames — подмножество wantSelfMetrics (по имени), которое обязано
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
// цепляет ни одно из текущих 54 пиннённых имён (gotcha_purge_queue_depth не
// совпадает — между "purge" и "depth" стоит "_queue_", а не пусто;
// gotcha_memory_limit_bytes не совпадает — "memory_limit" не однословно, а
// [a-z]+ подсистемы не захватывает "_").
var bareQueueSuffix = regexp.MustCompile(`^gotcha_[a-z]+_(depth|oldest_seconds|failed|capacity|bytes)$`)

// nonLiteralSelfMetricTypeMsg — текст ассерта на нелитеральный тип
// self-метрики, отдельно называющий dot-импорт: под ним тип метрики выглядит
// как голый идентификатор (Counter/Gauge) без какого-либо селектора вовсе, а
// не как "<localPkgName>.<Type>" — localPkgName в этом случае равен ".", и
// подстановка дала бы нечитаемое "..<Type>" вместо описания формы импорта.
func nonLiteralSelfMetricTypeMsg(localPkgName string) string {
	if localPkgName == "." {
		return "self-metric type is not a literal package selector — the file dot-imports selfmetrics, which turns the type into an unqualified identifier and makes it unrecognizable as a frozen literal"
	}
	return fmt.Sprintf("self-metric type is not a literal %s.<Type> selector — registering through a wrapper or a variable takes the metric out from under the name/type freeze", localPkgName)
}

// collectSelfMetrics — реально зарегистрированные в дереве self-метрики:
// имя → тип (первый строковый литерал и первый аргумент-селектор пакета
// selfmetrics пятиаргументного Add/AddInt), плюс сырое число найденных
// call-site'ов. Та же AST-детекция, что у TestSelfMetricsDocumented
// (selfmetrics_docs_test.go) — переиспользовать буквально нельзя, скан там
// приватен и заточен под сбор списка файлов регистрации, здесь нужны имя,
// тип и число вызовов.
//
// Нелитеральное имя (переменная, конкатенация, fmt.Sprintf) роняет тест
// сразу, с file:line: контракт замораживает конкретное имя, а не выражение,
// которое его в рантайме вычисляет, — со строкой-переменной обход дальше
// невозможен в принципе (t.Errorf, а не return true, — TestSelfMetricsDocumented
// проверяет то же самое независимо, но этот сторож не должен полагаться на
// соседний файл, чтобы не молчать в одиночку). Та же участь — расхождению
// типа при повторной регистрации одного имени: количество найденных
// call-site'ов также возвращается отдельно, чтобы вызывающий тест мог
// поймать «сторож ослеп» (сканер сломан или указывает не туда) отдельно от
// «золотой список устарел».
//
// Нелитеральный ТИП (первый аргумент — не селектор <pkg>.<Ident>, а
// переменная/параметр) в файле, реально импортирующем selfmetrics, роняет
// тест той же t.Errorf: обёртка вида
// func reg(r *selfmetrics.Registry, typ selfmetrics.Type, name string, …)
// пробрасывала бы typ дальше в r.Add/AddInt без единого литерального
// selfmetrics.Counter/Gauge на call-site'е — заморозка молча переставала бы
// действовать на всё, что регистрируется через такую обёртку. Пятиаргументный
// Add/AddInt чужого, не относящегося к selfmetrics типа в файле БЕЗ импорта
// пакета false positive не даёт: проверка ограничена importsSelfMetrics.
//
// <pkg> — не жёстко "selfmetrics": импорт под алиасом (import sm ".../
// internal/selfmetrics") меняет локальное имя, под которым файл ссылается на
// пакет, — сверка идёт с ФАКТИЧЕСКИМ локальным именем из этого же импорта
// (localPkgName), а не с зашитой строкой. Без этого файл с алиасом либо
// целиком выпадал бы из скана (метрики через sm.Counter выглядели бы как
// "чужой тип" и попадали бы в fail-open той же природы, что чинила правка 1,
// либо, если fail-open починен буквально, ложно считался бы нелитеральным
// типом — алиас так же легитимен, как и его отсутствие). Слепой (dot-)
// импорт (import "..."."/internal/selfmetrics") делает localPkgName равным
// ".", с которым ни один идентификатор совпасть не может, — тип метрики
// тогда обязан выглядеть как голый Counter/Gauge без селектора вовсе,
// call.Args[0] окажется *ast.Ident, а не *ast.SelectorExpr, и упадёт в ту же
// ветку "нелитеральный тип": слепой импорт делает исходный код
// нечитаемым для этого сканера настолько же, насколько и для человека.
//
// Предфильтр по телу файла — не по подстроке "selfmetrics." (её не даёт
// алиас: файл с "sm.Counter" физически не содержит "selfmetrics." нигде,
// кроме самой строки импорта), а по фрагменту ПУТИ импорта
// "internal/selfmetrics\"", который остаётся в тексте импорта независимо от
// локального имени — это только более дешёвая версия того же условия
// "импортирует ли файл этот пакет", а не альтернатива AST-проверке ниже.
func collectSelfMetrics(t *testing.T, tree *Tree) (map[string]string, int) {
	t.Helper()
	fset := token.NewFileSet()
	types := map[string]string{}
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
		// importsSelfMetrics/localPkgName снимаются с РЕАЛЬНОГО импорта файла
		// (f.Imports), а не с догадки по тексту: localPkgName — то имя,
		// которым файл в действительности ссылается на пакет (алиас, "." для
		// dot-импорта, иначе голое "selfmetrics").
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
			if prev, seen := types[name]; seen && prev != typ {
				t.Errorf("%s: self-metric %q is registered as selfmetrics.%s here, but as selfmetrics.%s elsewhere — a metric cannot change type between registrations", pos, name, typ, prev)
				return true
			}
			types[name] = typ
			return true
		})
	}
	return types, callSites
}

// TestSelfMetricNamesPinned — сверяет РЕАЛЬНО зарегистрированные self-метрики
// (имя и тип) с wantSelfMetrics в обе стороны.
func TestSelfMetricNamesPinned(t *testing.T) {
	tree := Load(t)
	live, callSites := collectSelfMetrics(t, tree)
	if callSites == 0 {
		t.Fatalf("blind guard: found 0 selfmetrics.Add/AddInt call-sites — the scan is looking at the wrong tree")
	}
	if callSites < len(wantSelfMetrics) {
		t.Fatalf("blind guard: found only %d call-sites, fewer than the %d pinned metrics — the scanner is broken",
			callSites, len(wantSelfMetrics))
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

// TestSelfMetricQueueNamingCanon — J1: любая метрика "глубина/возраст
// старейшего/отказы/ёмкость/байты" очереди обязана называться
// gotcha_<подсистема>_queue_<канон>, форма, в которой уже был purge.
func TestSelfMetricQueueNamingCanon(t *testing.T) {
	tree := Load(t)
	live, _ := collectSelfMetrics(t, tree)

	// Позитив: канонические имена очередей действительно существуют. Ловит
	// переименование МИМО канона (сегмент "_queue_" пропал) — общий пример
	// из брифа T6.
	for _, want := range wantQueueCanonNames {
		if _, ok := live[want]; !ok {
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
