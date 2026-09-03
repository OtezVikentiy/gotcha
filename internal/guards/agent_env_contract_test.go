package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Контракт переменных окружения GOTCHA_AGENT_* живёт в трёх независимых
// местах: сам разбор (internal/agent/config.go), команда установки, которую
// UI показывает пользователю (internal/web/hosts.go, agentInstallCommand), и
// install.sh, который эту команду исполняет. Ни одно из трёх раньше не
// сверялось с двумя другими — переименование переменной в config.go молча
// оставляло UI и install.sh врать пользователю старым именем, а появление
// новой переменной могло не долететь ни до одной из копий вовсе.
//
// hosts.go не генерируется из internal/agent и не импортирует его: internal/
// hostmetric/hostmetric.go документирует правило «web не должен тянуть
// gopsutil, агент не должен тянуть web» — internal/agent содержит probes.go в
// том же пакете, транзитивно тянущий gopsutil, а internal/web собирается в
// серверный бинарь. Импорт сломал бы это разделение ради констант. install.sh
// — shell, генерировать его из Go нельзя в принципе. Альтернатива — вынести
// имена в под-пакет без зависимостей (internal/agent/agentenv), общий для
// agent и web, — рассмотрена и отклонена: install.sh всё равно shell, такой
// пакет закрыл бы только копию в hosts.go, а не install.sh, то есть решил бы
// половину проблемы ценой лишнего пакета. Поэтому обе копии проверяются
// сторожом на СОВПАДЕНИЕ по содержимому (имя переменной, не номер строки), в
// ТРИ ШАГА:
//
//  1. found ⊆ canon — каждое имя GOTCHA_AGENT_*, встреченное в коде
//     hosts.go/install.sh (не в комментариях), обязано входить в
//     канонический набор из config.go. Переименование в config.go без правки
//     второй копии оставляет в ней старое имя, которого больше нет в
//     каноне, — тест падает.
//  2. mustAppear ⊆ found — каждое каноническое имя из явного, обоснованного
//     подмножества (mustAppearInHostsGo/mustAppearInInstallSh ниже) обязано
//     реально встретиться в коде копии. Без этого направления пропуск —
//     переменную протащили в копию, а потом убрали, — не ловился НИЧЕМ:
//     строка, которой в копии больше нет, не может сама себя не найти через
//     "found ⊆ canon".
//  3. классификация: каждое каноническое имя обязано быть либо в
//     mustAppearInHostsGo/InstallSh, либо в explicit-списке исключений с
//     обоснованием (hostsGoExclusions/installShExclusions). Без этого шага
//     шаг 2 бесполезен для НОВОЙ переменной: mustAppear — хардкод, и
//     добавление переменной в config.go само по себе никак не заставляет
//     человека дописать её туда же — сторож продолжал бы молчать до тех пор,
//     пока кто-то руками не решит завести на неё охрану (или забудет).
//     TestAgentEnvVarsClassified падает на самом факте появления
//     неклассифицированного канонического имени и требует решения —
//     "принадлежит копии" или "исключение с причиной" — вместо того, чтобы
//     позволить переменной остаться вне обеих системы отслеживания незамеченной.
//
// found ⊆ canon сравнивает по СТРОКОВЫМ ЛИТЕРАЛАМ разобранного go/ast
// hosts.go (не по регэкспу над сырым текстом файла с ручной вырезкой
// комментариев): agentInstallCommand строит команду в raw-строке на
// backtick'ах, а комментарии в AST не попадают вовсе — так класс ошибок
// «регэксп принял `//` внутри `https://` за начало комментария» не
// воспроизводим в принципе, а не закрывается более аккуратным ручным
// парсером кавычек. install.sh — не Go, AST для него нет; там применяется
// построчный шелл-стриппер (см. stripShellLineComments) — install.sh
// сегодня не содержит ни одного инлайн-"#"-комментария после кода (сверено
// вручную), полнострочного правила достаточно.
var agentEnvNameRe = regexp.MustCompile(`GOTCHA_AGENT_[A-Z_]+`)

// mustAppearInHostsGo — подмножество канона, обязанное встретиться в команде
// установки (hosts.go, agentInstallCommand): ровно две переменные,
// аутентифицирующие установку (endpoint инстанса и ключ проекта).
var mustAppearInHostsGo = []string{
	"GOTCHA_AGENT_ENDPOINT",
	"GOTCHA_AGENT_INGEST_KEY",
}

// hostsGoExclusions — остальные шесть канонических переменных, СОЗНАТЕЛЬНО не
// входящие в mustAppearInHostsGo, с обоснованием по каждой: все они —
// опциональные агентские настройки, не часть однострочной auth-only команды
// установки, которую показывает UI (задаются либо экспортом переменной перед
// запуском install.sh, либо правкой /etc/gotcha-agent/gotcha-agent.env после
// — internal/docs/{ru,en}/hosts.md).
var hostsGoExclusions = map[string]string{
	"GOTCHA_AGENT_HOSTNAME":                 "override host.name; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_CA_CERT":                  "путь к CA для самоподписанного инстанса; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_INTERVAL_SECONDS":         "интервал сбора; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY": "крайнее средство вместо CA_CERT; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_ENVIRONMENT":              "resource-метка deployment.environment; необязательная настройка агента, не часть auth-only команды UI",
	"GOTCHA_AGENT_ROLE":                     "resource-метка host.role; необязательная настройка агента, не часть auth-only команды UI",
}

// mustAppearInInstallSh — подмножество канона, которое install.sh обязан
// читать/писать в $CONF: ENDPOINT/KEY (обязательные, install-режим) плюс
// шесть опциональных, которые install.sh явно принимает через
// reject_newline/printf (internal/web/install.sh).
var mustAppearInInstallSh = []string{
	"GOTCHA_AGENT_ENDPOINT",
	"GOTCHA_AGENT_INGEST_KEY",
	"GOTCHA_AGENT_INTERVAL_SECONDS",
	"GOTCHA_AGENT_HOSTNAME",
	"GOTCHA_AGENT_CA_CERT",
	"GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY",
	"GOTCHA_AGENT_ENVIRONMENT",
	"GOTCHA_AGENT_ROLE",
}

// installShExclusions — канонические переменные, которых install.sh
// намеренно не читает и не пишет в $CONF; сегодня пуст (ENDPOINT/INGEST_KEY/
// INTERVAL_SECONDS/HOSTNAME/CA_CERT/TLS_INSECURE_SKIP_VERIFY/ENVIRONMENT/
// ROLE — все восемь канонических переменных — входят в
// mustAppearInInstallSh). Оставлен как
// map, а не удалён: TestAgentEnvVarsClassified требует классификации КАЖДОЙ
// новой канонической переменной либо сюда с причиной, либо в
// mustAppearInInstallSh — карта остаётся точкой, куда её вписать, если
// появится переменная, которую install.sh осознанно не должен принимать.
var installShExclusions = map[string]string{}

// agentConfigEnvVarSet — канонический набор имён, извлечённый из
// internal/agent/config.go тем же разбором go/ast, что и покрытие
// .env.example (collectGotchaEnvVars, env_example_test.go) — вторая
// независимая реализация здесь не заводится.
func agentConfigEnvVarSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	vars := collectGotchaEnvVars(t, root, filepath.Join("internal", "agent", "config.go"), nil)
	if len(vars) < 8 {
		t.Fatalf("обход ослеп: канонических переменных агента найдено %d, ожидалось не меньше 8", len(vars))
	}
	return vars
}

func containsName(list []string, name string) bool {
	for _, x := range list {
		if x == name {
			return true
		}
	}
	return false
}

// TestAgentEnvVarsClassified — шаг 3 контракта (см. докблок выше): каждое
// каноническое имя обязано быть классифицировано для КАЖДОЙ из двух копий —
// mustAppear или explicit-исключение с непустой причиной, третьего не дано.
// Появление новой переменной в internal/agent/config.go без правки списков
// этого файла роняет тест здесь, а не проходит незамеченным до тех пор, пока
// кто-то не решит завести на неё охрану вручную. Пустая причина
// (`hostsGoExclusions["X"] = ""`) тоже роняет тест: формально ключ есть, но
// докблок обещает обоснование, а не факт присутствия ключа в map — и сторож
// обязан требовать именно то, что обещает, а не то, что легче проверить.
// "n/a"/"TODO"-подобные заглушки НЕ отсекаются отдельной проверкой: их
// нельзя надёжно распознать регэкспом (человек так же легко напишет
// «см. выше» или обойдёт список стоп-слов), а разумность самой причины —
// предмет человеческого ревью классификации (см. ОГРАНИЧЕНИЕ ниже), которое
// этот тест и так не заменяет.
//
// ОГРАНИЧЕНИЕ: тест гарантирует ФАКТ классификации (имя есть в одном из двух
// списков с непустой причиной), а не её ПРАВИЛЬНОСТЬ. Если переменную по
// ошибке отнесли не в тот список — например, обязательную для install.sh
// переменную вписали в installShExclusions с правдоподобно звучащей, но
// неверной причиной, — тест этого не поймает: он не знает, что переменная
// «должна» быть mustAppear, он проверяет только «классифицирована ли она
// хоть как-то». Решение о корректности классификации остаётся за ревьюером
// задачи, которая её вносит — то самое молчаливое умолчание, из-за которого
// разрыв GOTCHA_AGENT_ENVIRONMENT/ROLE с install.sh пролежал незамеченным
// (аудит W3-G), записано здесь явно, а не оставлено неявным второй раз.
func TestAgentEnvVarsClassified(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	for name := range canon {
		inMust := containsName(mustAppearInHostsGo, name)
		reason, inExcl := hostsGoExclusions[name]
		switch {
		case inMust && inExcl:
			t.Errorf("%s одновременно в mustAppearInHostsGo и в hostsGoExclusions — противоречие, убрать из одного из двух", name)
		case !inMust && !inExcl:
			t.Errorf("%s — новая каноническая переменная internal/agent/config.go, не классифицированная для hosts.go: "+
				"добавить в mustAppearInHostsGo (если должна быть в команде установки) или в hostsGoExclusions с причиной (если не должна)", name)
		case inExcl && strings.TrimSpace(reason) == "":
			t.Errorf("hostsGoExclusions[%s] — пустая причина: исключение обязано объяснять, почему переменная не в mustAppearInHostsGo", name)
		}
	}
	for _, name := range mustAppearInHostsGo {
		if !canon[name] {
			t.Errorf("mustAppearInHostsGo содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
	for name := range hostsGoExclusions {
		if !canon[name] {
			t.Errorf("hostsGoExclusions содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}

	for name := range canon {
		inMust := containsName(mustAppearInInstallSh, name)
		reason, inExcl := installShExclusions[name]
		switch {
		case inMust && inExcl:
			t.Errorf("%s одновременно в mustAppearInInstallSh и в installShExclusions — противоречие, убрать из одного из двух", name)
		case !inMust && !inExcl:
			t.Errorf("%s — новая каноническая переменная internal/agent/config.go, не классифицированная для install.sh: "+
				"добавить в mustAppearInInstallSh (если должна быть в скрипте) или в installShExclusions с причиной (если не должна)", name)
		case inExcl && strings.TrimSpace(reason) == "":
			t.Errorf("installShExclusions[%s] — пустая причина: исключение обязано объяснять, почему install.sh её не пишет", name)
		}
	}
	for _, name := range mustAppearInInstallSh {
		if !canon[name] {
			t.Errorf("mustAppearInInstallSh содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
	for name := range installShExclusions {
		if !canon[name] {
			t.Errorf("installShExclusions содержит %s, которой нет в каноне internal/agent/config.go — список устарел", name)
		}
	}
}

// hostsGoAgentNames возвращает имена GOTCHA_AGENT_*, встреченные в СТРОКОВЫХ
// ЛИТЕРАЛАХ разобранного go/ast hosts.go — как в двойных кавычках, так и в
// raw-строках на backtick'ах (go/parser отдаёт содержимое BasicLit.Value для
// обоих видов одинаково надёжно, кавычки/backtick лексер уже разобрал
// правильно). Комментарии в AST не попадают вовсе (парсер вызван без
// parser.ParseComments), поэтому имя переменной в "//"-комментарии не может
// попасть в результат в принципе — в отличие от построчного регэксп-стриппера
// над сырым текстом, который путал "//" внутри raw-строки (agentInstallCommand
// строит команду с "https://" внутри backtick'ов) с началом комментария.
func hostsGoAgentNames(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "web", "hosts.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse hosts.go: %v", err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		names = append(names, agentEnvNameRe.FindAllString(lit.Value, -1)...)
		return true
	})
	return names
}

// stripShellLineComments убирает шелл-комментарии install.sh: любая строка,
// которая после обрезки пробелов начинается с "#", — комментарий целиком.
// install.sh сегодня не несёт ни одного инлайн-"#"-комментария после кода
// (сверено), поэтому построчного правила достаточно; сторож не должен путать
// имя переменной в комментарии с реальным использованием в коде.
func stripShellLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func TestHostsGoInstallCommandVarsMatchAgentConfig(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	found := hostsGoAgentNames(t, root)
	if len(found) < 2 {
		t.Fatalf("обход ослеп: в строковых литералах internal/web/hosts.go найдено %d упоминаний GOTCHA_AGENT_*, ожидалось не меньше 2", len(found))
	}
	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !canon[name] {
			t.Errorf("internal/web/hosts.go ссылается на %s, которой нет среди переменных internal/agent/config.go — "+
				"команду установки переименовали в одном месте и забыли другое", name)
		}
	}

	for _, name := range mustAppearInHostsGo {
		if !canon[name] {
			t.Fatalf("обход ослеп: mustAppearInHostsGo содержит %s, которой нет в каноне internal/agent/config.go — сам список устарел", name)
		}
		if !seen[name] {
			t.Errorf("internal/web/hosts.go больше не упоминает %s — переменную добавили/переименовали в internal/agent/config.go, "+
				"а команду установки не поправили следом", name)
		}
	}
}

func TestInstallShVarsMatchAgentConfig(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	canon := agentConfigEnvVarSet(t, root)

	raw, err := os.ReadFile(filepath.Join(root, "internal", "web", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	code := stripShellLineComments(string(raw))

	found := agentEnvNameRe.FindAllString(code, -1)
	if len(found) < 6 {
		t.Fatalf("обход ослеп: в коде install.sh (без комментариев) найдено %d упоминаний GOTCHA_AGENT_*, ожидалось не меньше 6", len(found))
	}
	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !canon[name] {
			t.Errorf("install.sh ссылается на %s, которой нет среди переменных internal/agent/config.go — "+
				"переименовали переменную в config.go и забыли install.sh", name)
		}
	}

	for _, name := range mustAppearInInstallSh {
		if !canon[name] {
			t.Fatalf("обход ослеп: mustAppearInInstallSh содержит %s, которой нет в каноне internal/agent/config.go — сам список устарел", name)
		}
		if !seen[name] {
			t.Errorf("install.sh больше не упоминает %s — переменную добавили/переименовали в internal/agent/config.go, "+
				"а install.sh не поправили следом", name)
		}
	}
}

// TestInstallShChecksBeforeSwappingBinary — install.sh обязан прогнать
// `--check` НОВЫМ бинарём из временного пути ($BIN.new) ДО того, как
// подменит боевой $BIN (`mv "$BIN.new" "$BIN"`): иначе на устаревшем имени
// переменной, сохранённом в $CONF (update-режим НЕ переписывает $CONF —
// см. main() в install.sh), systemd рано или поздно перезапустит уже
// подменённый бинарь на битом конфиге и получит `exit 2` —
// RestartPreventExitStatus=2 гасит юнит насмерть вместо "старый агент
// продолжает работать", как обещает upgrade.md (ops-review E3 T8 круг 1,
// реалистичный триггер — плановая перезагрузка хоста между обновлением
// сервера и обходом хостов). Проверяет ПОРЯДОК двух операций в файле, а не
// факт их присутствия — TestInstallShVarsMatchAgentConfig выше уже
// проверяет имена; здесь достаточно строкового индекса, переустановка
// блоков местами обязана уронить именно этот тест.
func TestInstallShChecksBeforeSwappingBinary(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "internal", "web", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	code := stripShellLineComments(string(raw))

	checkIdx := strings.Index(code, `"$BIN.new" --check`)
	mvIdx := strings.Index(code, `mv "$BIN.new" "$BIN"`)
	if checkIdx < 0 {
		t.Fatal(`обход ослеп: install.sh не содержит вызов "$BIN.new" --check`)
	}
	if mvIdx < 0 {
		t.Fatal(`обход ослеп: install.sh не содержит mv "$BIN.new" "$BIN"`)
	}
	if mvIdx < checkIdx {
		t.Errorf(`install.sh подменяет боевой бинарь (mv "$BIN.new" "$BIN") РАНЬШЕ, чем проверяет конфиг ("$BIN.new" --check) — на устаревших именах в $CONF это гасит юнит на первом же рестарте вместо того, чтобы оставить работать старый бинарь`)
	}
}
