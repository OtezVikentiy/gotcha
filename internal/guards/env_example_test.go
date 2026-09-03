package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envReaderFuncs — функции cmd/gotcha/config.go, читающие переменные
// окружения. Имя переменной — первый строковый литерал GOTCHA_* в аргументах
// (у optionalBoolEnv имя стоит вторым аргументом, поэтому «первый аргумент»
// недостаточен). go/ast, а не регэксп — чтобы не ловить имена в текстах
// ошибок и комментариях.
var envReaderFuncs = map[string]bool{
	"str":             true,
	"intNum":          true,
	"num":             true,
	"boolEnv":         true,
	"boolEnvDef":      true,
	"optionalBoolEnv": true,
	"getenv":          true,
}

// numericReaderFuncs — подмножество envReaderFuncs, читающее переменную как
// голое число (strconv.ParseInt), а не строку/duration-строку/bool. Именно
// эти переменные подлежат конвенции единиц измерения ниже: значение вида
// "60" само по себе не несёт единицы, в отличие от "30s"
// (GOTCHA_AGENT_INTERVAL, time.ParseDuration) или булева флага.
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
	// numericVars собирает и из агентского файла: сейчас там нет голых
	// числовых чтений (GOTCHA_AGENT_INTERVAL — duration-строка вида "30s"
	// через time.ParseDuration, единица уже в значении, не в имени), но если
	// агент когда-нибудь заведёт intNum/num-подобный читатель, конвенция
	// подхватит его тем же путём, без отдельной правки теста.
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
