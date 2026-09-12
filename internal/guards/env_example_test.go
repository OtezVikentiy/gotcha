package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// Имя переменной — первый строковый литерал GOTCHA_* в аргументах, не
// обязательно первый позиционно. go/ast, не regex — не ловит имена в текстах ошибок/комментариях.
var envReaderFuncs = map[string]bool{
	"str":        true,
	"strGuarded": true,
	"intNum":     true,
	"num":        true,
	"boolEnv":    true,
	"boolEnvDef": true,
	"getenv":     true,
	"parseBool":  true,
}

// Значение читается как голое число ("60"), не duration-строка ("30s") или
// bool — поэтому единица измерения обязана быть в имени, а не в значении.
var numericReaderFuncs = map[string]bool{
	"intNum": true,
	"num":    true,
}

var unitSuffixes = []string{"_SECONDS", "_DAYS", "_HOURS", "_BYTES", "_PER_SEC", "_PER_MIN"}

// Закрытый список переменных-счётчиков без единицы измерения — осознанная
// граница конвенции, не обход; каждая запись обязана нести обоснование.
var unitlessCounters = map[string]string{
	"GOTCHA_SMTP_PORT":                 "номер сетевого порта, не измеряемая величина",
	"GOTCHA_ALERT_BUDGET_LIMIT":        "лимит числа алертов в окне — штука",
	"GOTCHA_CARDINALITY_LIMIT":         "лимит числа уникальных серий метрик — штука",
	"GOTCHA_NOTIFY_CONCURRENCY":        "число параллельных воркеров доставки уведомлений — штука",
	"GOTCHA_UPTIME_CONCURRENCY":        "число параллельных проверок аптайма — штука",
	"GOTCHA_DEFAULT_EVENT_QUOTA":       "квота — число событий в периоде, штука",
	"GOTCHA_DEFAULT_TRANSACTION_QUOTA": "квота — число транзакций в периоде, штука",
	"GOTCHA_DEFAULT_METRIC_QUOTA":      "квота — число точек метрик в периоде, штука",
	"GOTCHA_DEFAULT_PROFILE_QUOTA":     "квота — число профилей в периоде, штука",
	"GOTCHA_DEFAULT_LOG_QUOTA":         "квота — число строк логов в периоде, штука",
	"GOTCHA_EXPORT_MAX_ROWS":           "лимит числа строк в выгрузке, штука",
}

func hasUnitSuffix(name string) bool {
	for _, suf := range unitSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// Токены-анти-паттерны, устранённые таблицей переименований (envcontract.Renamed) —
// квалификатор подсистемы не может нести ни один из них отдельным "_"-сегментом.
var booleanCanonForbiddenTokens = map[string]bool{
	"RUN": true, "ENABLE": true, "DISABLE": true, "DISABLED": true,
	"USE": true, "ON": true, "OFF": true, "FLAG": true, "TOGGLE": true,
	"ALLOW": true, "AUTO": true,
}

// Каноничная форма — одна из трёх: *_ENABLED; <подсистема>_ALLOW_<послабление>
// (обе части непустые); или квалификатор внутри подсистемы, реально существующей в known.
func hasBooleanCanonForm(name string, known map[string]bool) bool {
	rest := strings.TrimPrefix(name, "GOTCHA_")
	if strings.HasSuffix(name, "_ENABLED") {
		return true
	}
	if idx := strings.Index(rest, "_ALLOW_"); idx > 0 && idx+len("_ALLOW_") < len(rest) {
		return true
	}
	tokens := strings.Split(rest, "_")
	for _, tok := range tokens {
		if booleanCanonForbiddenTokens[tok] {
			return false
		}
	}
	if len(tokens) == 0 || tokens[0] == "" {
		return false
	}
	prefix := "GOTCHA_" + tokens[0] + "_"
	for other := range known {
		if other == name {
			continue
		}
		if strings.HasPrefix(other, prefix) {
			return true
		}
	}
	return false
}

// Булевы читатели: boolEnv/boolEnvDef и прямой parseBool в cmd/gotcha/config.go,
// а также единственный булев читатель internal/agent/config.go.
var boolReaderFuncs = map[string]bool{
	"boolEnv":    true,
	"boolEnvDef": true,
	"parseBool":  true,
}

func collectBoolReaderVars(t *testing.T, root, relFile string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relFile), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relFile, err)
	}
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if !boolReaderFuncs[name] {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v := strings.Trim(lit.Value, `"`)
			if strings.HasPrefix(v, "GOTCHA_") {
				vars[v] = true
			}
			break
		}
		return true
	})
	return vars
}

// numericOut, если не nil, дополнительно получает подмножество имён,
// прочитанных через numericReaderFuncs — им и только им нужна единица в имени.
func collectGotchaEnvVars(t *testing.T, root, relFile string, numericOut map[string]bool) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relFile), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relFile, err)
	}
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if !envReaderFuncs[name] {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v := strings.Trim(lit.Value, `"`)
			if strings.HasPrefix(v, "GOTCHA_") {
				vars[v] = true
				if numericOut != nil && numericReaderFuncs[name] {
					numericOut[v] = true
				}
				break
			}
		}
		return true
	})
	return vars
}

// Без ограничения по префиксу GOTCHA_ — для переменных вроде GOMEMLIMIT,
// которые стандартизует сам Go-рантайм.
func collectOSEnvVars(t *testing.T, root, relFile string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relFile), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relFile, err)
	}
	vars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		if sel.Sel.Name != "LookupEnv" && sel.Sel.Name != "Getenv" {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			vars[strings.Trim(lit.Value, `"`)] = true
			break
		}
		return true
	})
	return vars
}

// Заякорено на начало строки: имя, процитированное в третьей колонке
// (перекрёстная ссылка в описании), не должно засчитываться как документирующее.
var tableRowFirstCellRe = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]*)`")

// Строка с несколькими именами в одной ячейке через `/` не распознаётся —
// такие сокращения переписаны отдельными строками в configuration.md.
func tableVarNames(doc string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(doc, "\n") {
		m := tableRowFirstCellRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if name == "GOMEMLIMIT" || strings.HasPrefix(name, "GOTCHA_") {
			out[name] = true
		}
	}
	return out
}

// ru и en проверяются раздельно двумя циклами, не одним объединённым
// множеством — иначе дыра в одной локали маскируется полнотой другой.
func checkConfigurationTableParity(t testingT, vars, readers, ruTable, enTable map[string]bool) {
	t.Helper()
	for v := range vars {
		if !ruTable[v] {
			t.Errorf("ru: %s читается кодом (или это GOMEMLIMIT) и есть в .env.example, но не задокументирована строкой таблицы в internal/docs/ru/configuration.md", v)
		}
		if !enTable[v] {
			t.Errorf("en: %s читается кодом (или это GOMEMLIMIT) и есть в .env.example, но не задокументирована строкой таблицы в internal/docs/en/configuration.md", v)
		}
	}
	for v := range ruTable {
		if !readers[v] {
			t.Errorf("ru: %s задокументирована строкой таблицы в configuration.md, но её не читает ни cmd/gotcha/config.go, ни internal/agent/config.go, ни Docker Compose (не несёт префикс GOTCHA_COMPOSE_/GOTCHA_BUILD_) — переменная-призрак", v)
		}
	}
	for v := range enTable {
		if !readers[v] {
			t.Errorf("en: %s задокументирована строкой таблицы в configuration.md, но её не читает ни cmd/gotcha/config.go, ни internal/agent/config.go, ни Docker Compose (не несёт префикс GOTCHA_COMPOSE_/GOTCHA_BUILD_) — переменная-призрак", v)
		}
	}
}

// Единица измерения обязательна в имени любой numericReaderFuncs-переменной —
// единственный легальный обход: запись в unitlessCounters с обоснованием.
func TestEnvExampleCoversConfig(t *testing.T) {
	tree := Load(t)

	numericVars := map[string]bool{}
	serverVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go"), numericVars)
	if len(serverVars) < 20 {
		t.Fatalf("collected only %d server variables — cmd/gotcha/config.go parsing is broken", len(serverVars))
	}
	if len(numericVars) < 30 {
		t.Fatalf("collected only %d numeric variables — cmd/gotcha/config.go parsing is broken, or numericReaderFuncs (intNum/num) stopped being used", len(numericVars))
	}
	// numericVars собирает числа из обоих файлов — агентский intNum-читатель
	// попадает под ту же конвенцию без отдельной правки теста.
	agentVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("internal", "agent", "config.go"), numericVars)
	if len(agentVars) < 8 {
		t.Fatalf("collected only %d agent variables — internal/agent/config.go parsing is broken", len(agentVars))
	}
	// GOMEMLIMIT читается напрямую через os.LookupEnv, без префикса GOTCHA_.
	memlimitVars := collectOSEnvVars(t, tree.Root, filepath.Join("internal", "memlimit", "memlimit.go"))
	if len(memlimitVars) < 1 {
		t.Fatalf("collected 0 os.LookupEnv/os.Getenv variables from internal/memlimit/memlimit.go — parsing is broken, or the GOMEMLIMIT read moved/was removed")
	}

	vars := map[string]bool{}
	for v := range serverVars {
		vars[v] = true
	}
	for v := range agentVars {
		vars[v] = true
	}
	for v := range memlimitVars {
		vars[v] = true
	}

	example, err := os.ReadFile(filepath.Join(tree.Root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for v := range vars {
		// Ищем «NAME=», не голое вхождение имени: короткое имя — префикс
		// длинного, упоминание длинного дало бы ложный зелёный короткому.
		if !strings.Contains(string(example), v+"=") {
			t.Errorf("%s is read by config.go but missing from .env.example", v)
		}
	}

	for v := range numericVars {
		if hasUnitSuffix(v) {
			continue
		}
		if _, ok := unitlessCounters[v]; ok {
			continue
		}
		t.Errorf("%s is read as a bare number (intNum/num) but its name carries no unit suffix (one of %s) and is not listed in unitlessCounters — naming convention violation", v, strings.Join(unitSuffixes, "/"))
	}

	// Сторож сам ловит устаревшую запись, которую больше никто не читает как число.
	for v := range unitlessCounters {
		if !numericVars[v] {
			t.Errorf("%s is listed in unitlessCounters but is no longer read as a bare number — remove the stale exception", v)
		}
	}

	// tableVars — серверные переменные плюс GOMEMLIMIT, без агентских (документированы отдельно).
	tableVars := map[string]bool{}
	for v := range serverVars {
		tableVars[v] = true
	}
	for v := range memlimitVars {
		tableVars[v] = true
	}

	// Их читатель — сам Compose, не Go-код — легитимно документируются таблицей,
	// не будучи в vars/tableVars.
	composeVars := map[string]bool{}
	for _, name := range []string{"docker-compose.yml", "docker-compose.small.yml"} {
		raw, err := os.ReadFile(filepath.Join(tree.Root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range composeSubstRe.FindAllStringSubmatch(string(raw), -1) {
			composeVars[m[1]] = true
		}
	}
	if len(composeVars) < 8 {
		t.Fatalf("collected only %d compose-substituted GOTCHA_COMPOSE_*/GOTCHA_BUILD_* variables — compose parsing is broken", len(composeVars))
	}

	readers := map[string]bool{}
	for v := range vars { // server + agent + GOMEMLIMIT, уже собранные выше
		readers[v] = true
	}
	for v := range composeVars {
		readers[v] = true
	}

	ruDoc, err := os.ReadFile(filepath.Join(tree.Root, "internal", "docs", "ru", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	enDoc, err := os.ReadFile(filepath.Join(tree.Root, "internal", "docs", "en", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	ruTable := tableVarNames(string(ruDoc))
	enTable := tableVarNames(string(enDoc))
	if len(ruTable) < 50 || len(enTable) < 50 {
		t.Fatalf("collected %d ru / %d en table rows from configuration.md — table parsing is broken (tableRowFirstCellRe stopped matching)", len(ruTable), len(enTable))
	}

	checkConfigurationTableParity(t, tableVars, readers, ruTable, enTable)
}

// boolEnv/getenv/parseBool исключены — нет литерального дефолта одним
// значением (implicit false, голая строка, или трёхстабильная логика GOTCHA_EVALUATORS_ENABLED).
var configDefaultReaders = map[string]bool{
	"str":        true,
	"strGuarded": true,
	"intNum":     true,
	"num":        true,
	"boolEnvDef": true,
}

// Дефолт — не литерал, а идентификатор другой переменной (defQuota/ssrfAll);
// сверяется ниже с types.ExprString, иначе устаревшая запись молчала бы.
var nonLiteralConfigDefaults = map[string]string{
	"GOTCHA_DEFAULT_EVENT_QUOTA":         "defQuota",
	"GOTCHA_DEFAULT_TRANSACTION_QUOTA":   "defQuota",
	"GOTCHA_DEFAULT_METRIC_QUOTA":        "defQuota",
	"GOTCHA_DEFAULT_PROFILE_QUOTA":       "defQuota",
	"GOTCHA_DEFAULT_LOG_QUOTA":           "defQuota",
	"GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME":   "ssrfAll",
	"GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK":  "ssrfAll",
	"GOTCHA_SSRF_ALLOW_PRIVATE_OIDC":     "ssrfAll",
	"GOTCHA_SSRF_ALLOW_PRIVATE_TELEGRAM": "ssrfAll",
}

// .env.example показывает не текущий дефолт кода, а рабочий пример: 0 —
// сентинел «авто», не размер; "" у TELEGRAM_API_BASE — дефолтный URL.
var exampleOnlyConfigDefaults = map[string]string{
	"GOTCHA_MAX_WRITER_BUFFER_BYTES": "0",
	"GOTCHA_MAX_INGEST_QUEUE_BYTES":  "0",
	"GOTCHA_TELEGRAM_API_BASE":       "",
}

// literal валиден только при ok: true.
type configDefault struct {
	literal string
	ok      bool
	expr    string
}

// Только строки/числа/bool и битовый сдвиг `1<<20` — унарный минус и +/-/*
// не заведены: таких дефолтов в config.go нет, а невыполнимую ветку не покрыть мутацией.
func evalConstExpr(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		switch e.Kind {
		case token.STRING:
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				return "", false
			}
			return s, true
		case token.INT:
			n, err := strconv.ParseInt(strings.ReplaceAll(e.Value, "_", ""), 0, 64)
			if err != nil {
				return "", false
			}
			return strconv.FormatInt(n, 10), true
		}
		return "", false
	case *ast.Ident:
		if e.Name == "true" || e.Name == "false" {
			return e.Name, true
		}
		return "", false
	case *ast.BinaryExpr:
		if e.Op != token.SHL {
			return "", false
		}
		lv, lok := evalConstExpr(e.X)
		rv, rok := evalConstExpr(e.Y)
		if !lok || !rok {
			return "", false
		}
		ln, err1 := strconv.ParseInt(lv, 10, 64)
		rn, err2 := strconv.ParseInt(rv, 10, 64)
		if err1 != nil || err2 != nil {
			return "", false
		}
		return strconv.FormatInt(ln<<uint(rn), 10), true
	}
	return "", false
}

// Ограничено cmd/gotcha/config.go нарочно: в internal/agent/config.go у
// intNum/parseBool другая сигнатура — второй аргумент там уже сырое значение, не дефолт.
func collectConfigDefaults(t *testing.T, root, relFile string) map[string]configDefault {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, relFile), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relFile, err)
	}
	out := map[string]configDefault{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || !configDefaultReaders[fun.Name] || len(call.Args) != 2 {
			return true
		}
		keyLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			return true
		}
		key := strings.Trim(keyLit.Value, `"`)
		if !strings.HasPrefix(key, "GOTCHA_") {
			return true
		}
		lit, litOK := evalConstExpr(call.Args[1])
		out[key] = configDefault{literal: lit, ok: litOK, expr: types.ExprString(call.Args[1])}
		return true
	})
	// boolEnv(key) не входит в configDefaultReaders — у него нет второго
	// аргумента, дефолт (false) зашит в саму функцию, собирается отдельно.
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || fun.Name != "boolEnv" || len(call.Args) != 1 {
			return true
		}
		keyLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			return true
		}
		key := strings.Trim(keyLit.Value, `"`)
		if !strings.HasPrefix(key, "GOTCHA_") {
			return true
		}
		if _, exists := out[key]; !exists {
			out[key] = configDefault{literal: "false", ok: true, expr: "false (boolEnv implicit default)"}
		}
		return true
	})
	return out
}

// Якорь на начало строки: "GOTCHA_SSRF_ALLOW_PRIVATE=" не префикс строки
// "GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME=true" — короткое имя не ловит чужую строку.
func envExampleLineValue(lines []string, name string) (string, bool) {
	plain := name + "="
	commented := "#" + name + "="
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, plain); ok {
			return v, true
		}
		if v, ok := strings.CutPrefix(line, commented); ok {
			return v, true
		}
	}
	return "", false
}

// TestEnvExampleCoversConfig выше сверяет только имя переменной, не значение —
// эта проверка ловит расхождение дефолта в коде с примером в .env.example.
func TestEnvExampleDefaultsMatchConfig(t *testing.T) {
	tree := Load(t)
	defaults := collectConfigDefaults(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go"))
	if len(defaults) < 20 {
		t.Fatalf("collected only %d config defaults — cmd/gotcha/config.go parsing is broken, or configDefaultReaders stopped matching", len(defaults))
	}

	example, err := os.ReadFile(filepath.Join(tree.Root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	exampleLines := strings.Split(string(example), "\n")

	checked := 0
	for name, def := range defaults {
		if wantExpr, isNonLiteral := nonLiteralConfigDefaults[name]; isNonLiteral {
			if def.expr != wantExpr {
				t.Errorf("%s: nonLiteralConfigDefaults records its default expression as %q, but cmd/gotcha/config.go now has %q (ok=%v) — update or remove the stale entry", name, wantExpr, def.expr, def.ok)
			}
			continue
		}
		if !def.ok {
			t.Errorf("%s: default expression %q is not a recognized literal (string/bool/int, optionally shifted) — extend evalConstExpr, or add it to nonLiteralConfigDefaults with a reason if it is genuinely computed at runtime from another variable", name, def.expr)
			continue
		}
		if wantLiteral, isExampleOnly := exampleOnlyConfigDefaults[name]; isExampleOnly {
			if wantLiteral != def.literal {
				t.Errorf("%s: exampleOnlyConfigDefaults records the code default as %q, but cmd/gotcha/config.go now has %q — update the exception (and re-check whether .env.example's worked example next to it still makes sense)", name, wantLiteral, def.literal)
			}
			continue
		}

		value, found := envExampleLineValue(exampleLines, name)
		if !found {
			// TestEnvExampleCoversConfig уже проверяет и валит на отсутствии
			// строки — падать здесь тем же диагнозом второй раз незачем.
			continue
		}
		checked++
		if value != def.literal {
			t.Errorf("%s: default in cmd/gotcha/config.go is %q, but .env.example has %q — keep them in sync (a stand seeded from .env.example must land on the same settings an unset env would)", name, def.literal, value)
		}
	}

	for name := range nonLiteralConfigDefaults {
		if _, ok := defaults[name]; !ok {
			t.Errorf("%s: listed in nonLiteralConfigDefaults but is no longer read via a 2-arg reader in cmd/gotcha/config.go — remove the stale entry", name)
		}
	}
	for name := range exampleOnlyConfigDefaults {
		if _, ok := defaults[name]; !ok {
			t.Errorf("%s: listed in exampleOnlyConfigDefaults but is no longer read via a 2-arg reader in cmd/gotcha/config.go — remove the stale entry", name)
		}
	}

	if checked < 15 {
		t.Fatalf("checked only %d variables against .env.example — value-sync coverage looks suspiciously low, is envExampleLineValue matching lines at all?", checked)
	}
}

func TestUnitSuffixConvention(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"GOTCHA_ESCALATION_INTERVAL_SECONDS", true},
		{"GOTCHA_EVENT_RETENTION_DAYS", true},
		{"GOTCHA_PROJECT_PURGE_RECONCILE_HOURS", true},
		{"GOTCHA_MAX_EVENT_BYTES", true},
		{"GOTCHA_INGEST_RATE_PER_SEC", true},
		{"GOTCHA_DIST_RATE_PER_MIN", true},
		{"GOTCHA_ALERT_BUDGET_LIMIT", false},
		{"GOTCHA_NOTIFY_CONCURRENCY", false},
		{"GOTCHA_SMTP_PORT", false},
		{"GOTCHA_ESCALATION_INTERVAL", false},
	}
	for _, c := range cases {
		if got := hasUnitSuffix(c.name); got != c.want {
			t.Errorf("hasUnitSuffix(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// Синтетический known-набор — держит только то, что нужно кейсам ниже, для
// контролируемой проверки условия (b) формы 3, не завязанной на envcontract.Known.
var booleanCanonFixtureKnown = map[string]bool{
	"GOTCHA_HSTS_ENABLED":         true,
	"GOTCHA_HSTS_MAX_AGE_SECONDS": true,
	"GOTCHA_HSTS_PRELOAD":         true,
	"GOTCHA_SCRUB_EMAIL":          true,
	"GOTCHA_SCRUB_IP":             true,
	// Намеренно единственный представитель своей "подсистемы" — условие (b) формы 3 не выполняется.
	"GOTCHA_SOMETHING": true,
}

func TestBooleanNamingConvention(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"GOTCHA_HSTS_ENABLED", true},
		{"GOTCHA_EVALUATORS_ENABLED", true},
		{"GOTCHA_AUTO_MIGRATE_ENABLED", true},
		{"GOTCHA_SECRET_KEY_ALLOW_INSECURE", true},
		{"GOTCHA_SSRF_ALLOW_PRIVATE", true},
		{"GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME", true},
		// Форма 3: проходит, потому что known несёт другие имена той же подсистемы.
		{"GOTCHA_HSTS_INCLUDE_SUBDOMAINS", true},
		{"GOTCHA_SCRUB_FREETEXT", true},
		// Изолирует условие (a) от (b): подсистема HSTS реальна, но токен ON
		// запрещён — без этого случая токен-запрет было бы нечем поймать отдельно.
		{"GOTCHA_HSTS_ON", false},
		{"GOTCHA_RUN_SOMETHING", false},
		{"GOTCHA_ALLOW_X", false},
		{"GOTCHA_SOMETHING", false},
		// ALLOW_ на самом краю имени: нет послабления справа, форма 2 не
		// признаётся; токен ALLOW дополнительно режет и форму 3.
		{"GOTCHA_ALLOW_", false},
	}
	for _, c := range cases {
		if got := hasBooleanCanonForm(c.name, booleanCanonFixtureKnown); got != c.want {
			t.Errorf("hasBooleanCanonForm(%q, ...) = %v, want %v", c.name, got, c.want)
		}
	}
}

// Подключён к реальным булевым читателям и envcontract.Known, без exception-map.
func TestBooleanNamingConventionRealReaders(t *testing.T) {
	tree := Load(t)

	vars := map[string]bool{}
	for v := range collectBoolReaderVars(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go")) {
		vars[v] = true
	}
	for v := range collectBoolReaderVars(t, tree.Root, filepath.Join("internal", "agent", "config.go")) {
		vars[v] = true
	}
	if len(vars) < 15 {
		t.Fatalf("collected only %d boolean variables — parsing is broken, or boolEnv/boolEnvDef/parseBool stopped being used", len(vars))
	}

	for v := range vars {
		if !hasBooleanCanonForm(v, envcontract.Known) {
			t.Errorf("%s is read as a boolean but matches neither *_ENABLED, nor <subsystem>_ALLOW_<relaxation>, nor an established subsystem qualifier — expected form *_ENABLED or <subsystem>_ALLOW_<relaxation>", v)
		}
	}
}

func TestMemlimitEnvVarDiscovered(t *testing.T) {
	tree := Load(t)
	vars := collectOSEnvVars(t, tree.Root, filepath.Join("internal", "memlimit", "memlimit.go"))
	if !vars["GOMEMLIMIT"] {
		t.Fatalf("collectOSEnvVars(internal/memlimit/memlimit.go) = %v, want it to contain GOMEMLIMIT", vars)
	}
}

// Иначе новая переменная без правки envcontract.Known стала бы «неизвестной»
// для checkUnknownEnvVars и валила бы старт легитимному оператору.
func TestKnownEnvVarsCoversConfig(t *testing.T) {
	tree := Load(t)
	serverVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go"), nil)
	if len(serverVars) < 20 {
		t.Fatalf("collected only %d server variables — cmd/gotcha/config.go parsing is broken", len(serverVars))
	}
	agentVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("internal", "agent", "config.go"), nil)
	if len(agentVars) < 8 {
		t.Fatalf("collected only %d agent variables — internal/agent/config.go parsing is broken", len(agentVars))
	}
	for v := range serverVars {
		if !envcontract.Known[v] {
			t.Errorf("%s is read by cmd/gotcha/config.go but missing from envcontract.Known", v)
		}
	}
	for v := range agentVars {
		if !envcontract.Known[v] {
			t.Errorf("%s is read by internal/agent/config.go but missing from envcontract.Known", v)
		}
	}
}

// Без этого Known мог бы знать имена-призраки и замаскировать реальную
// опечатку оператора, случайно совпавшую с призраком.
func TestKnownEnvVarsHaveNoGhosts(t *testing.T) {
	tree := Load(t)
	serverVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("cmd", "gotcha", "config.go"), nil)
	agentVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("internal", "agent", "config.go"), nil)
	for v := range envcontract.Known {
		if !serverVars[v] && !agentVars[v] {
			t.Errorf("envcontract.Known contains %s, but neither cmd/gotcha/config.go nor internal/agent/config.go reads it — stale/ghost entry", v)
		}
	}
}

// Имя, упомянутое в прозе или третьей колонке, покрытием не считается;
// GOMEMLIMIT (без префикса GOTCHA_) тоже подхватывается.
func TestTableVarNamesFirstColumnOnly(t *testing.T) {
	doc := "Проза упоминает `GOTCHA_PROSE_ONLY` мимоходом, не как строку таблицы.\n" +
		"\n" +
		"| Переменная | По умолчанию | Описание |\n" +
		"|---|---|---|\n" +
		"| `GOTCHA_REAL_ROW` | `1` | Ссылается на `GOTCHA_CROSS_REF` в тексте описания. |\n" +
		"| `GOMEMLIMIT` | *(вычисляется)* | Рантайм Go. |\n"

	got := tableVarNames(doc)

	for _, want := range []string{"GOTCHA_REAL_ROW", "GOMEMLIMIT"} {
		if !got[want] {
			t.Errorf("tableVarNames: %s не найден, хотя это первая колонка настоящей строки таблицы", want)
		}
	}
	for _, notWant := range []string{"GOTCHA_PROSE_ONLY", "GOTCHA_CROSS_REF"} {
		if got[notWant] {
			t.Errorf("tableVarNames: %s засчитан, хотя это упоминание в прозе/третьей колонке, не строка таблицы", notWant)
		}
	}
	if len(got) != 2 {
		t.Errorf("tableVarNames вернул %v, ожидалось ровно 2 имени", got)
	}
}

// ru и en проверяются раздельными циклами — дыра в одной локали не должна
// маскироваться полнотой другой; оба подтеста ниже падают независимо.
func TestConfigurationTableParityCatchesMissingRow(t *testing.T) {
	t.Run("ru", func(t *testing.T) {
		ft := &fakeT{}
		vars := map[string]bool{"GOTCHA_FAKE_VAR": true}
		readers := vars
		ruTable := map[string]bool{} // GOTCHA_FAKE_VAR отсутствует
		enTable := map[string]bool{"GOTCHA_FAKE_VAR": true}
		checkConfigurationTableParity(ft, vars, readers, ruTable, enTable)
		ft.requireFailure(t, "GOTCHA_FAKE_VAR")
		ft.requireFailure(t, "ru:")
	})
	t.Run("en", func(t *testing.T) {
		ft := &fakeT{}
		vars := map[string]bool{"GOTCHA_FAKE_VAR": true}
		readers := vars
		ruTable := map[string]bool{"GOTCHA_FAKE_VAR": true}
		enTable := map[string]bool{} // GOTCHA_FAKE_VAR отсутствует
		checkConfigurationTableParity(ft, vars, readers, ruTable, enTable)
		ft.requireFailure(t, "GOTCHA_FAKE_VAR")
		ft.requireFailure(t, "en:")
	})
}

// Обратное направление: строка таблицы без читателя в коде (ни код, ни
// compose) — тоже находка, красная в обеих локалях.
func TestConfigurationTableParityCatchesGhostRow(t *testing.T) {
	ft := &fakeT{}
	vars := map[string]bool{}
	readers := map[string]bool{} // GOTCHA_GHOST_VAR без читателя
	ruTable := map[string]bool{"GOTCHA_GHOST_VAR": true}
	enTable := map[string]bool{"GOTCHA_GHOST_VAR": true}
	checkConfigurationTableParity(ft, vars, readers, ruTable, enTable)
	ft.requireFailure(t, "GOTCHA_GHOST_VAR")
}

// Переменная compose-неймспейса (читатель — подстановка ${...} в docker-compose.yml,
// не Go-код) не должна считаться призраком, даже не входя в vars.
func TestConfigurationTableParityAcceptsComposeReader(t *testing.T) {
	ft := &fakeT{}
	vars := map[string]bool{}
	readers := map[string]bool{"GOTCHA_COMPOSE_FAKE": true} // читатель — compose, не код
	ruTable := map[string]bool{"GOTCHA_COMPOSE_FAKE": true}
	enTable := map[string]bool{"GOTCHA_COMPOSE_FAKE": true}
	checkConfigurationTableParity(ft, vars, readers, ruTable, enTable)
	if ft.failed {
		t.Fatalf("compose-переменная с читателем в readers ошибочно забракована: %v", ft.msgs)
	}
}
