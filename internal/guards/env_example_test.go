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

// envReaderFuncs — функции cmd/gotcha/config.go, читающие переменные
// окружения. Имя переменной — первый строковый литерал GOTCHA_* в аргументах
// (не обязательно первый аргумент позиционно — только это делает разбор
// устойчивым к сигнатуре конкретного читателя). go/ast, а не регэксп — чтобы
// не ловить имена в текстах ошибок и комментариях.
//
// strGuarded добавлен задачей 9 (M2): без него GOTCHA_PG_DSN/
// GOTCHA_CH_DSN/GOTCHA_SECRET_KEY — реальные поля cmd/gotcha.Config — не
// попадали в собранный набор, и TestComposeVarsNamespaced (compose_test.go)
// не мог структурно отличить их форвардинг (`GOTCHA_SECRET_KEY:
// ${GOTCHA_SECRET_KEY:-...}`) от настоящей compose-only переменной под тем
// же именем ключа — сигнатура (key, def string) совпадает со str/boolEnvDef,
// разбор общий.
// parseBool добавлен задачей 10 (E3, реестр известных имён):
// без него GOTCHA_EVALUATORS_ENABLED — читается напрямую через
// parseBool("GOTCHA_EVALUATORS_ENABLED"), не через boolEnv/boolEnvDef,
// у RunEvaluators особая тристабильная логика (nil/true/false) — не
// попадала в собранный набор ни здесь, ни в internal/envcontract.Known,
// хотя реально читается cmd/gotcha/config.go.
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

// numericReaderFuncs — подмножество envReaderFuncs, читающее переменную как
// голое число (strconv.ParseInt), а не строку/duration-строку/bool. Именно
// эти переменные подлежат конвенции единиц измерения ниже: значение вида
// "60" само по себе не несёт единицы, в отличие от duration-строки вида
// "30s" (time.ParseDuration, единица прямо в значении) или булева флага.
var numericReaderFuncs = map[string]bool{
	"intNum": true,
	"num":    true,
}

// unitSuffixes — конвенция именования числовых переменных: единица измерения
// прямо в имени, а не подразумевается контекстом (см. параграф «Конвенция
// именования переменных» в internal/docs/{ru,en}/configuration.md).
var unitSuffixes = []string{"_SECONDS", "_DAYS", "_HOURS", "_BYTES", "_PER_SEC", "_PER_MIN"}

// unitlessCounters — закрытый список переменных, которые читаются через
// numericReaderFuncs, но по своей природе считают штуки, а не время/объём/
// частоту: единицы измерения у них нет и появиться не может. Это НЕ обход
// конвенции — это её осознанная граница. Каждая строка обязана нести
// обоснование; правка этого списка обязана быть заметна в код-ревью, это и
// есть её цель (см. докблок TestEnvExampleCoversConfig).
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

// hasUnitSuffix — истинно, если имя переменной оканчивается одной из
// unitSuffixes. Вынесено из TestEnvExampleCoversConfig отдельной функцией,
// чтобы конвенция проверялась и юнит-тестом на фиксированных именах
// (TestUnitSuffixConvention), а не только сквозным разбором config.go.
func hasUnitSuffix(name string) bool {
	for _, suf := range unitSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// booleanCanonForbiddenTokens — задача 11, п.2, решение владельца
// (2026-09-03, вариант (a), механический критерий БЕЗ exception-map):
// токены-анти-паттерны, которые устранила таблица переименований релиза 2
// (envcontract.Renamed, «E3, заморозка контракта») — GOTCHA_RUN_EVALUATORS
// (RUN), GOTCHA_AUTO_MIGRATE (AUTO), GOTCHA_ALLOW_INSECURE_SECRET (голый
// ALLOW без подсистемы), опечатка-кандидат GOTCHA_HSTS_ENABLE (ENABLE вместо
// ENABLED). Имя-квалификатор подсистемы (форма 3 в hasBooleanCanonForm ниже)
// не имеет права нести ни один из этих токенов отдельным "_"-сегментом —
// иначе оно в точности повторяет один из устранённых анти-паттернов.
var booleanCanonForbiddenTokens = map[string]bool{
	"RUN": true, "ENABLE": true, "DISABLE": true, "DISABLED": true,
	"USE": true, "ON": true, "OFF": true, "FLAG": true, "TOGGLE": true,
	"ALLOW": true, "AUTO": true,
}

// hasBooleanCanonForm — задача 11, п.2, решение владельца (2026-09-03,
// вариант (a)): каноничное булево имя — ОДНА из трёх форм, без
// exception-map:
//
//  1. `*_ENABLED` — включение функции целиком.
//  2. `<подсистема>_ALLOW_<послабление>` — единственный маркер послабления
//     безопасности в словаре (GOTCHA_SSRF_ALLOW_PRIVATE*,
//     GOTCHA_SECRET_KEY_ALLOW_INSECURE). Подсистема и послабление ОБЯЗАНЫ
//     быть непустыми: GOTCHA_ALLOW_X — красный, подсистемы нет ("_ALLOW_"
//     ищется в остатке имени ПОСЛЕ префикса GOTCHA_, не во всём имени —
//     иначе сам префикс GOTCHA_ сходил бы за «подсистему»).
//  3. Квалификатор ВНУТРИ уже существующей подсистемы — не форма 1/2, но
//     обязан пройти оба условия: (a) ни один "_"-токен имени не входит в
//     booleanCanonForbiddenTokens; (b) первый токен после GOTCHA_ реально
//     общий хотя бы с ОДНИМ ДРУГИМ именем known (истина — реестр
//     envcontract.Known на настоящем скане, не отдельный список) —
//     подсистема должна реально существовать в словаре, а не быть
//     выдумана: голое GOTCHA_SOMETHING такую проверку не проходит,
//     GOTCHA_HSTS_INCLUDE_SUBDOMAINS — проходит (в реестре есть
//     GOTCHA_HSTS_ENABLED/_MAX_AGE_SECONDS/_PRELOAD).
//
// На HEAD (см. envcontract.Known) все булевы имена проходят одну из трёх
// форм — «нулём исключений» выполняется буквально, без списка. Шесть имён
// живут только формой 3 (не формой 1/2): GOTCHA_HSTS_INCLUDE_SUBDOMAINS,
// GOTCHA_HSTS_PRELOAD, GOTCHA_SCRUB_IP, GOTCHA_SCRUB_EMAIL,
// GOTCHA_SCRUB_FREETEXT, GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY — владелец
// решает отдельно, переименовывать ли их в форму *_ENABLED/_ALLOW_ этой же
// волной (вне рамок этой задачи, см. отчёт задачи 11).
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

// boolReaderFuncs — читатели булевых значений: boolEnv/boolEnvDef в
// cmd/gotcha/config.go (обёртки над parseBool — см. envReaderFuncs выше) и
// прямой parseBool — и в cmd/gotcha/config.go (GOTCHA_EVALUATORS_ENABLED,
// минуя boolEnv/boolEnvDef, см. докблок envReaderFuncs), и в
// internal/agent/config.go (единственный булев читатель агента).
var boolReaderFuncs = map[string]bool{
	"boolEnv":    true,
	"boolEnvDef": true,
	"parseBool":  true,
}

// collectBoolReaderVars — как collectGotchaEnvVars выше, но фильтр по
// boolReaderFuncs, а не envReaderFuncs: конвенция булевых имён сканирует
// РЕАЛЬНЫЕ булевы читатели, а не любые GOTCHA_*-читатели вообще (строковые/
// числовые/duration-переменные конвенции не подлежат). Отдельная функция, а
// не третий out-параметр у collectGotchaEnvVars (как numericOut) — там уже
// два разных источника (server/agent) вызываются с разными наборами
// нужных выходов, и совмещение усложнило бы сигнатуру всех вызывающих мест
// без нужды: то же решение, каким collectOSEnvVars уже сосуществует рядом
// как отдельный целевой сканер.
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

// collectGotchaEnvVars разбирает один Go-файл и возвращает имена переменных
// GOTCHA_*, читаемых через envReaderFuncs — тот же приём (go/ast, а не
// регэксп), что и раньше, вынесенный в функцию, потому что источников теперь
// два: cmd/gotcha/config.go (сервер) и internal/agent/config.go (агент,
// getenv-параметр LoadConfig подходит под то же имя "getenv"). Дублировать
// разбор под каждый источник — плодить два места, которые разъедутся
// синтаксисом так же, как разъехались сами копии контракта (см.
// agent_env_contract_test.go).
//
// numericOut, если не nil, дополнительно получает подмножество имён,
// прочитанных через numericReaderFuncs (intNum/num) — переменных, значение
// которых голое число без собственной единицы измерения. Именно это
// подмножество, а не весь результат функции, проверяется конвенцией единиц
// в TestEnvExampleCoversConfig: строковые/duration-строковые/булевы
// переменные конвенции не подлежат вовсе.
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

// collectOSEnvVars разбирает один Go-файл и возвращает имена ЛЮБЫХ
// переменных окружения (без ограничения по префиксу GOTCHA_), прочитанных
// напрямую через os.LookupEnv/os.Getenv — приём для переменных вроде
// GOMEMLIMIT (internal/memlimit/memlimit.go), которые сам Go-рантайм
// стандартизует под своим именем и product-префикс на них не распространяется.
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

// tableRowFirstCellRe находит ПЕРВУЮ ячейку строки markdown-таблицы, если
// она несёт код-цитату вида `NAME`, сразу за начальным `|` (не считая
// пробелов). Заякорено на начало строки нарочно: имя, процитированное где-то
// в третьей колонке (перекрёстная ссылка на другую переменную в тексте
// описания — так уже было с `_CLIENT_SECRET` в OIDC-строке, задача 11),
// не должно засчитываться как документирующая эту переменную
// строка — только первая колонка таблицы документирует.
var tableRowFirstCellRe = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]*)`")

// tableVarNames возвращает множество имён GOTCHA_*/GOMEMLIMIT, задокументированных
// ПЕРВОЙ колонкой строки markdown-таблицы в doc — упоминание имени в прозе
// (в том числе в описании соседней строки) не считается. Одно имя на
// ячейку: строка с несколькими именами в одной ячейке через `/`
// (сокращение вида “ `GOTCHA_OIDC_CLIENT_ID` / `_CLIENT_SECRET` “) не
// распознаётся — такие строки переписаны отдельными строками на переменную
// (см. configuration.md обеих локалей, раздел OAuth/SSO), а не добавлением
// более умного разбора: сокращение само по себе плохо читается человеком,
// чинить парсер под него — тащить проблему дальше вместо того, чтобы убрать
// её из документа.
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

// checkConfigurationTableParity — задача 11, пункт 1: паритет .env.example
// ↔ таблицы configuration.md обеих локалей (звено код ↔ .env.example уже
// проверено выше, в TestEnvExampleCoversConfig). Две независимые сверки:
//
//   - vars (переменные, реально читаемые cmd/gotcha/config.go, плюс
//     GOMEMLIMIT) обязаны быть строкой таблицы в ОБЕИХ локалях — ru и en
//     проверяются раздельно, каждая своим циклом, а не одним объединённым
//     множеством имён: дыра в одной локали не должна маскироваться полнотой
//     другой (мутация, которую держит в уме ревьюер задачи — убрать сверку
//     по одной из локалей — красит именно этот раздельный цикл, а не
//     тихо проходит по union);
//   - обратное — «строка таблицы без читателя в коде — тоже находка»:
//     каждое имя, задокументированное строкой таблицы, обязано иметь
//     читателя из readers (объединение серверных/агентских переменных,
//     GOMEMLIMIT и compose-неймспейса GOTCHA_COMPOSE_*/GOTCHA_BUILD_* —
//     решение владельца, задача 11 п.5: их читатель — подстановка `${...}` в
//     docker-compose.yml, не Go-код). Без читателя ни там, ни там — строка
//     документирует переменную-призрак.
//
// Агентские переменные (internal/agent/config.go) намеренно НЕ входят в
// vars: они документированы отдельной таблицей на странице /docs/hosts
// (см. upgrade.md, «Актуальные имена ... в справочнике переменных на
// странице Хосты»), а не в configuration.md — дублировать их сюда значило
// бы держать вторую копию той таблицы, которая разъедется с первой при
// следующей агентской переменной. Как ЧИТАТЕЛЬ (readers) агентские
// переменные всё же учитываются — иначе строка о них в configuration.md
// (если такая когда-нибудь появится) ложно считалась бы призраком.
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

// TestEnvExampleCoversConfig — №86: каждая переменная GOTCHA_*, которую
// читает cmd/gotcha/config.go ИЛИ internal/agent/config.go, обязана
// упоминаться в .env.example — единственном полном справочном файле
// переменных в репозитории. Исключений нет: переменные добавлялись в конфиг
// и не попадали в справочный файл повторно, класс закрывается сверкой, а не
// дисциплиной. internal/agent/config.go добавлен отдельно (аудит W3-G #2):
// восемь агентских переменных не были покрыты ни .env.example, ни этим
// сторожем — сторож видел только cmd/gotcha/config.go.
//
// Конвенция единиц измерения (T1, усилена после находки A1): по умолчанию
// ЛЮБАЯ переменная, прочитанная как голое число (numericReaderFuncs —
// intNum/num), обязана нести единицу измерения в имени. Раньше конвенция
// проверялась только для переменных, прочитанных через отдельный алиас
// durNum — и та же переменная, добавленная через intNum, проходила гейт
// зелёной: конвенция держалась на том, что автор вспомнит пометить чтение
// «нужным» именем функции. Теперь подозрительна любая числовая переменная
// без юнита, а не помеченная особым образом; единственный легальный обход —
// явная запись в unitlessCounters с обоснованием рядом (штука, а не
// время/объём/частота). durNum как отдельный маркер убран из
// cmd/gotcha/config.go: после инверсии умолчания решение не зависит от
// того, каким именем читатель назван, так что маркер, оставленный в коде,
// ничего бы не гарантировал.
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
	// numericVars собирает и из агентского файла: internal/agent/config.go
	// (E3 T8) завёл собственную intNum-обёртку для GOTCHA_AGENT_INTERVAL_SECONDS
	// (переименована из duration-строки GOTCHA_AGENT_INTERVAL/time.ParseDuration
	// — единица теперь в имени, не в значении), и конвенция подхватывает её
	// тем же путём, без отдельной правки этого теста — ровно то поведение,
	// которое комментарий выше и предполагал заранее.
	agentVars := collectGotchaEnvVars(t, tree.Root, filepath.Join("internal", "agent", "config.go"), numericVars)
	if len(agentVars) < 8 {
		t.Fatalf("collected only %d agent variables — internal/agent/config.go parsing is broken", len(agentVars))
	}
	// internal/memlimit/memlimit.go читает GOMEMLIMIT напрямую через
	// os.LookupEnv, без префикса GOTCHA_ — сверка ниже фильтровала по этому
	// префиксу и не видела эту переменную вовсе (T2, ops-A9).
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
		// Ищем «NAME=» (значением или закомментированным примером), а не голое
		// вхождение имени: короткое имя — префикс длинного
		// (GOTCHA_SSRF_ALLOW_PRIVATE ⊂ GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME), и
		// упоминание длинного давало бы ложный зелёный короткому.
		if !strings.Contains(string(example), v+"=") {
			t.Errorf("%s is read by config.go but missing from .env.example", v)
		}
	}

	// Конвенция единиц измерения (T1): любая переменная, прочитанная как
	// голое число (numericVars), обязана нести единицу в имени, если она не
	// перечислена явно в unitlessCounters. См. докблок функции выше.
	for v := range numericVars {
		if hasUnitSuffix(v) {
			continue
		}
		if _, ok := unitlessCounters[v]; ok {
			continue
		}
		t.Errorf("%s is read as a bare number (intNum/num) but its name carries no unit suffix (one of %s) and is not listed in unitlessCounters — naming convention violation", v, strings.Join(unitSuffixes, "/"))
	}

	// Список исключений обязан быть закрытым и актуальным: строка, которую
	// больше никто не читает как число (переменная удалена, переименована с
	// добавлением юнита, или её чтение сменилось на durNum-подобный текст со
	// своей единицей в значении), — мёртвый груз, который тихо расширяет
	// разрешённый список мимо реального кода. Ревьюер задачи это поймает
	// мутацией; сторож ловит это сам, при любом следующем прогоне.
	for v := range unitlessCounters {
		if !numericVars[v] {
			t.Errorf("%s is listed in unitlessCounters but is no longer read as a bare number — remove the stale exception", v)
		}
	}

	// Задача 11, пункт 1: паритет .env.example ↔ таблицы configuration.md
	// обеих локалей (см. докблок checkConfigurationTableParity). tableVars —
	// СЕРВЕРНЫЕ переменные плюс GOMEMLIMIT, БЕЗ агентских (документированы
	// отдельно, на /docs/hosts — см. докблок checkConfigurationTableParity):
	// сборка из уже собранных выше serverVars/memlimitVars, а не второй
	// проход разбора.
	tableVars := map[string]bool{}
	for v := range serverVars {
		tableVars[v] = true
	}
	for v := range memlimitVars {
		tableVars[v] = true
	}

	// composeVars — переменные подстановки Docker Compose (${GOTCHA_COMPOSE_*}/
	// ${GOTCHA_BUILD_*}) из обоих compose-файлов: их читатель — сам Compose,
	// не Go-код (решение владельца, задача 11 п.5), они легитимно документируются
	// таблицей, не будучи в vars/tableVars. composeSubstRe и её семантика —
	// compose_test.go (тот же пакет).
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

// configDefaultReaders — подмножество envReaderFuncs cmd/gotcha/config.go, у
// которых второй аргумент — буквальный дефолт, записанный прямо в месте
// вызова (str/strGuarded/intNum/num/boolEnvDef). boolEnv/getenv/parseBool
// сознательно исключены: у boolEnv нет второго аргумента вовсе (implicit
// default — false, см. отдельный проход в collectConfigDefaults ниже);
// getenv/parseBool — не «дефолт в одно значение» в принципе, и восемь
// переменных, читаемых ими напрямую, этой сверке не подлежат вовсе (имя
// каждой всё равно проверяет соседний TestEnvExampleCoversConfig):
// GOTCHA_SECRET_KEY_PREV, GOTCHA_SCRUB_DENY_KEYS, GOTCHA_SCRUB_KEEP_KEYS,
// GOTCHA_TRUSTED_PROXIES, GOTCHA_TRUSTED_RECIPIENTS — голый getenv(),
// "не задано" это буквально пустая строка без дальнейшей интерпретации;
// GOTCHA_LOGGING_LEVEL/_FORMAT — тоже голый getenv(), но пустая строка
// («не задано») равнозначна "info"/"text" только ВНУТРИ setupLogging
// (cmd/gotcha/main.go), а не текстуально — сверять "" с "info" одним
// равенством означало бы падать на корректном коде; GOTCHA_EVALUATORS_ENABLED
// читается голым parseBool() напрямую и специально тристабилен
// (nil/true/false, см. докблок RunEvaluators в loadConfig) — «не задано»
// это отдельное состояние, а не синоним false, зафиксировать его одним
// литералом-дефолтом нельзя.
var configDefaultReaders = map[string]bool{
	"str":        true,
	"strGuarded": true,
	"intNum":     true,
	"num":        true,
	"boolEnvDef": true,
}

// nonLiteralConfigDefaults — переменные, чей второй аргумент в
// configDefaultReaders — не литерал, а идентификатор другой переменной,
// посчитанной раньше в loadConfig из другого env/издания: defQuota (от
// GOTCHA_EDITION — oss даёт 0, saas — 1_000_000) и ssrfAll (от
// GOTCHA_SSRF_ALLOW_PRIVATE, каскадный дефолт для четырёх per-path
// оверрайдов). У них нет единственного «правильного» значения вне
// рантюма конкретной инсталляции, поэтому закомментированная строка рядом
// в .env.example документирует ПРИМЕР переопределения (см. комментарии в
// файле), а не сам дефолт — сверять их текстом с .env.example нечего.
//
// Список закрыт и сверяется ниже с реальным выражением дефолта
// (types.ExprString): если дефолт в коде перестал быть этим идентификатором
// (переменную переименовали, дефолт стал литералом) — запись обязана
// обновиться, иначе тест тихо продолжит пропускать переменную, которая уже
// может проверяться как обычный литерал.
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

// exampleOnlyConfigDefaults — переменные с обычным литеральным дефолтом в
// коде, у которых закомментированная строка .env.example сознательно
// показывает не этот дефолт, а рабочий пример значения (см. комментарий
// в .env.example рядом с каждой):
//
//   - GOTCHA_MAX_WRITER_BUFFER_BYTES/GOTCHA_MAX_INGEST_QUEUE_BYTES — 0 внутри
//     Config это сентинел «нет явного потолка, взять автоматический» (см.
//     докблок num() в loadConfig), а не размер в байтах; .env.example
//     показывает готовый потолок, который имеет смысл раскомментировать.
//   - GOTCHA_TELEGRAM_API_BASE — "" означает "https://api.telegram.org" (см.
//     .env.example: «Empty means https://api.telegram.org»); закомментированная
//     строка показывает, на что заменить дефолт при отсутствии прямого egress.
//
// Значение здесь — ожидаемый ТЕКУЩИЙ дефолт кода (не значение из
// .env.example): проверка ниже требует его совпадения с
// evalConstExpr(defaultArg), так что если кто-то поменяет дефолт в коде
// (например, задаст ненулевой потолок по умолчанию), запись здесь устареет
// заметно, а не тихо продолжит выдавать зелёный на устаревшем сравнении.
var exampleOnlyConfigDefaults = map[string]string{
	"GOTCHA_MAX_WRITER_BUFFER_BYTES": "0",
	"GOTCHA_MAX_INGEST_QUEUE_BYTES":  "0",
	"GOTCHA_TELEGRAM_API_BASE":       "",
}

// configDefault — дефолт одной переменной GOTCHA_*, как его определяет
// вызов reader'а в cmd/gotcha/config.go. literal валиден только при ok:
// true; expr — types.ExprString второго аргумента вызова, нужен и для
// диагностики, и для сверки nonLiteralConfigDefaults на актуальность.
type configDefault struct {
	literal string
	ok      bool
	expr    string
}

// evalConstExpr сворачивает буквальные дефолты str/strGuarded/intNum/num/
// boolEnvDef в текст, напрямую сравнимый со значением после "=" в
// .env.example: строковые и булевы литералы (true/false — предопределённые
// идентификаторы Go, не BasicLit), целые (включая "_"-разделители — Go
// принимает 200_000, .env.example пишет 200000 — и битовый сдвиг влево
// 1<<20, единственная небуквальная арифметика дефолтов в cmd/gotcha/config.go
// на сегодня). Идентификатор, отличный от true/false (defQuota, ssrfAll), —
// не литерал: ok=false, обработка — в nonLiteralConfigDefaults у вызывающего.
//
// Только эти формы: унарный минус и +/-/* здесь нарочно не заведены —
// ни одного дефолта с такой формой в cmd/gotcha/config.go нет, ветка «про
// запас» никогда не исполнилась бы ни одним реальным вызовом, а
// невыполнимую ветку не покрыть мутацией. Появится такой дефолт — тест
// упадёт на "not a recognized literal" (TestEnvExampleDefaultsMatchConfig
// ниже), и это тот момент, когда сюда стоит добавить нужный case.
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

// collectConfigDefaults разбирает cmd/gotcha/config.go и возвращает, для
// каждой переменной GOTCHA_*, прочитанной через configDefaultReaders или
// голый boolEnv(), её дефолт. Источник истины — сам вызов ридера, а не
// список, набранный в этом файле руками: TestEnvExampleCoversConfig выше
// уже проверял имена тем же приёмом (go/ast вместо второй копии, которая
// разъедется с кодом) — TestEnvExampleDefaultsMatchConfig ниже проверяет их
// значения тем же способом.
//
// Ограничено cmd/gotcha/config.go нарочно, без internal/agent/config.go:
// там собственные intNum/parseBool с другой сигнатурой (name, raw string) —
// второй аргумент там уже прочитанное сырое значение, а не дефолт, — и все
// восемь агентских переменных читаются голым getenv() без второго
// аргумента вовсе. Сверять там значения тем же кодом означало бы либо
// неверно трактовать "raw" как дефолт, либо молча ничего не проверять —
// понятнее явно ограничить область функции, чем притворяться, что она
// работает универсально.
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
	// boolEnv(key) не входит в configDefaultReaders — структурно у него нет
	// второго аргумента, который можно было бы прочитать, а не потому что у
	// него нет дефолта: дефолт есть (false), он просто зашит в саму функцию
	// (`v, _ := parseBool(key); return v`; parseBool на "" отдаёт (false,
	// false)), а не в место вызова — собирается отдельным проходом с
	// фиксированным значением.
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

// envExampleLineValue ищет СТРОКУ (не произвольное вхождение через
// strings.Contains, как в TestEnvExampleCoversConfig выше — там достаточно
// доказать присутствие имени, здесь нужно снять точное значение), которая
// начинается с "NAME=" или "#NAME=", и возвращает всё после "=". Префиксный
// якорь на всю строку сохраняет то же свойство, что и Contains(v+"=") выше:
// "GOTCHA_SSRF_ALLOW_PRIVATE=" не является префиксом строки
// "GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME=true" — короткое имя не ловит чужую
// строку длинного.
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

// TestEnvExampleDefaultsMatchConfig — K5-3: TestEnvExampleCoversConfig выше
// сверяет только ИМЯ переменной в .env.example, но не её значение — если
// дефолт в loadConfig разъедется с примером в .env.example (кто-то поправит
// один файл и забудет другой), тот сторож промолчит, а оператор,
// поднимающий стенд из .env.example, тихо получит чужие настройки вместо
// документированных дефолтов. Источник истины — collectConfigDefaults,
// разбирающий САМ вызов ридера в cmd/gotcha/config.go, а не вторая копия
// значений, набранная в этом тесте руками (см. её докблок).
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

// TestUnitSuffixConvention — юнит-тест конвенции единиц измерения (T1) на
// фиксированных именах, независимый от фактического набора переменных в
// config.go: проверяет саму функцию hasUnitSuffix, а не только её эффект
// внутри сквозного разбора TestEnvExampleCoversConfig. Покрывает все шесть
// суффиксов конвенции и характерные отрицательные случаи — голый счётчик
// (LIMIT/CONCURRENCY) и срок без единицы в имени.
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
		// Регрессия конкретного бага, который эта конвенция закрывает:
		// голый срок без единицы в имени.
		{"GOTCHA_ESCALATION_INTERVAL", false},
	}
	for _, c := range cases {
		if got := hasUnitSuffix(c.name); got != c.want {
			t.Errorf("hasUnitSuffix(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// booleanCanonFixtureKnown — синтетический "known"-набор для
// TestBooleanNamingConvention: держит только то, что нужно кейсам ниже,
// чтобы (b)-условие формы 3 (реальная семья в словаре) проверялось на
// вымышленных, но контролируемых данных, а не на envcontract.Known
// (реальный реестр гоняет отдельный TestBooleanNamingConventionRealReaders
// ниже).
var booleanCanonFixtureKnown = map[string]bool{
	"GOTCHA_HSTS_ENABLED":         true,
	"GOTCHA_HSTS_MAX_AGE_SECONDS": true,
	"GOTCHA_HSTS_PRELOAD":         true,
	"GOTCHA_SCRUB_EMAIL":          true,
	"GOTCHA_SCRUB_IP":             true,
	// GOTCHA_SOMETHING — намеренно единственный представитель своей
	// "подсистемы": ни один ДРУГОЙ ключ этого набора не начинается с
	// GOTCHA_SOMETHING_, поэтому условие (b) формы 3 не выполняется.
	"GOTCHA_SOMETHING": true,
}

// TestBooleanNamingConvention — задача 11, п.2, решение владельца
// (2026-09-03): конвенция булевых имён на фиксированных кейсах, по образцу
// TestUnitSuffixConvention выше — независимая от фактического набора
// переменных в config.go проверка самой функции-классификатора (реальный
// скан — TestBooleanNamingConventionRealReaders ниже). Все три кейса
// контракта задачи 11 дословно: GOTCHA_RUN_SOMETHING (форма 1/2 не
// подходит, форбидден-токен RUN — анти-паттерн GOTCHA_RUN_EVALUATORS),
// GOTCHA_ALLOW_X (форма 2 не подходит — подсистемы перед ALLOW_ нет, и
// форбидден-токен ALLOW тоже режет форму 3), голое GOTCHA_SOMETHING (форма
// 3 не подходит — нет ДРУГОГО известного имени той же "подсистемы").
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
		// Форма 3 (квалификатор существующей подсистемы): проходит, потому
		// что booleanCanonFixtureKnown несёт ДРУГИЕ имена той же подсистемы
		// HSTS/SCRUB.
		{"GOTCHA_HSTS_INCLUDE_SUBDOMAINS", true},
		{"GOTCHA_SCRUB_FREETEXT", true},
		// Изолирует условие (a) от условия (b): подсистема HSTS реальна в
		// фикстуре (siblings есть), но токен ON запрещён — без отдельной
		// проверки (a) эта форма прошла бы мимо запрета одним лишь наличием
		// подсистемы.
		{"GOTCHA_HSTS_ON", false},
		// Контракт задачи 11, п.2 — три дословных негативных кейса.
		{"GOTCHA_RUN_SOMETHING", false},
		{"GOTCHA_ALLOW_X", false},
		{"GOTCHA_SOMETHING", false},
		// ALLOW_ на самом краю имени — нет послабления справа (idx+len(marker)
		// упирается в конец остатка), форма 2 не признаётся; форбидден-токен
		// ALLOW тоже режет форму 3.
		{"GOTCHA_ALLOW_", false},
	}
	for _, c := range cases {
		if got := hasBooleanCanonForm(c.name, booleanCanonFixtureKnown); got != c.want {
			t.Errorf("hasBooleanCanonForm(%q, ...) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBooleanNamingConventionRealReaders — задача 11, п.2, решение владельца
// (2026-09-03, вариант (a)): сторож подключён к РЕАЛЬНЫМ булевым читателям
// (boolEnv/boolEnvDef/parseBool — collectBoolReaderVars, cmd/gotcha/config.go
// и internal/agent/config.go), БЕЗ exception-map. На HEAD все булевы имена
// обязаны проходить hasBooleanCanonForm относительно настоящего
// envcontract.Known — «нулём исключений» проверяется буквально, а не на
// фикстуре.
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

// TestMemlimitEnvVarDiscovered — T2 (ops-A9): без collectOSEnvVars сторож
// .env.example фильтровал по strings.HasPrefix(v, "GOTCHA_") и был слеп к
// GOMEMLIMIT (internal/memlimit/memlimit.go), стандартной переменной
// Go-рантайма без продуктового префикса. Юнит-тест на сам разбор, отдельно
// от сквозной проверки .env.example в TestEnvExampleCoversConfig — если
// разбор перестанет находить GOMEMLIMIT (переименование чтения, перенос в
// другой файл), это укажет само по себе, а не потонет в общем "not found".
func TestMemlimitEnvVarDiscovered(t *testing.T) {
	tree := Load(t)
	vars := collectOSEnvVars(t, tree.Root, filepath.Join("internal", "memlimit", "memlimit.go"))
	if !vars["GOMEMLIMIT"] {
		t.Fatalf("collectOSEnvVars(internal/memlimit/memlimit.go) = %v, want it to contain GOMEMLIMIT", vars)
	}
}

// TestKnownEnvVarsCoversConfig — E3 T10: реестр известных имён
// envcontract.Known обязан быть НАДмножеством всего, что реально читают
// cmd/gotcha/config.go и internal/agent/config.go — иначе новая переменная,
// добавленная в конфиг без правки Known, стала бы «неизвестной» для
// checkUnknownEnvVars (cmd/gotcha/config.go) и валила бы старт легитимному
// оператору. Источник истины — тот же AST-сборщик collectGotchaEnvVars, что
// уже сверяет .env.example выше в TestEnvExampleCoversConfig — не вторая
// копия разбора.
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

// TestKnownEnvVarsHaveNoGhosts — обратная сверка: каждое имя в
// envcontract.Known обязано реально читаться cmd/gotcha/config.go или
// internal/agent/config.go. Без этого теста Known мог бы «знать» имена-
// призраки (опечатка при ручном заведении записи, оставшееся после
// переименования старое имя) — checkUnknownEnvVars пропустил бы реальную
// опечатку оператора, случайно совпавшую с призраком, и реестр перестал бы
// быть надёжной проверкой в обе стороны, которой его делает
// TestKnownEnvVarsCoversConfig выше.
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

// TestTableVarNamesFirstColumnOnly — tableVarNames считает документирующей
// только ПЕРВУЮ колонку строки таблицы: имя, упомянутое в прозе или в
// третьей колонке (описании) той же или другой строки, покрытием не
// считается (контракт задачи 11, п.1: «упоминание имени в прозе не
// считается покрытием»). GOMEMLIMIT — легитимное отдельное имя без префикса
// GOTCHA_, тоже подхватывается.
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

// TestConfigurationTableParityCatchesMissingRow — «подсунуть переменную,
// отсутствующую в таблице RU» из контракта задачи 11, п.1: переменная есть
// в vars (код+.env.example), но отсутствует строкой в ru-таблице — красный
// с именем переменной и локалью в тексте ошибки. Симметричный случай для en
// — сама мутация, которую держит в уме ревьюер («убрать сверку по одной из
// локалей»): реализация проверяет ru и en раздельными циклами (см. докблок
// checkConfigurationTableParity), поэтому дыра в одной локали не маскируется
// полнотой другой — оба подтеста ниже должны падать независимо.
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

// TestConfigurationTableParityCatchesGhostRow — обратное направление
// контракта: строка таблицы без читателя в коде — тоже находка. Имя
// присутствует в обеих таблицах, но отсутствует в readers (ни серверный/
// агентский код, ни compose-неймспейс его не читает) — красный в обеих
// локалях.
func TestConfigurationTableParityCatchesGhostRow(t *testing.T) {
	ft := &fakeT{}
	vars := map[string]bool{}
	readers := map[string]bool{} // GOTCHA_GHOST_VAR без читателя
	ruTable := map[string]bool{"GOTCHA_GHOST_VAR": true}
	enTable := map[string]bool{"GOTCHA_GHOST_VAR": true}
	checkConfigurationTableParity(ft, vars, readers, ruTable, enTable)
	ft.requireFailure(t, "GOTCHA_GHOST_VAR")
}

// TestConfigurationTableParityAcceptsComposeReader — решение владельца,
// задача 11 п.5: переменная compose-неймспейса (читатель — подстановка ${...} в
// docker-compose.yml, не Go-код) НЕ должна считаться призраком, даже не
// входя в vars. Регрессия на случай, если кто-то однажды сузит readers до
// одного vars, забыв про composeVars.
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
