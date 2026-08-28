package guards

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// renamedEnvVars — старые имена переменных окружения (переименованы этой
// волной, cmd/gotcha/config.go и internal/agent/config.go их больше не
// читают) → новые. Переименование уже один раз оставило за собой настоящий
// баг: internal/web/probes.go рисовал пользователю в интерфейсе команду
// `docker run -e GOTCHA_SERVER_URL=…`, которой после переименования не
// существовало — скопированная команда молча не работала бы, и поймал это
// человек, а не гейт. Этот сторож закрывает класс находки целиком: ни одно
// из десяти старых имён не должно встречаться нигде в дереве (кроме
// CHANGELOG — см. докблок TestNoRenamedEnvVarNames).
var renamedEnvVars = map[string]string{
	"GOTCHA_METRIC_EVAL_INTERVAL":    "GOTCHA_METRIC_EVAL_INTERVAL_SECONDS",
	"GOTCHA_PROFILE_EVAL_INTERVAL":   "GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS",
	"GOTCHA_HOST_EVAL_INTERVAL":      "GOTCHA_HOST_EVAL_INTERVAL_SECONDS",
	"GOTCHA_SLO_EVAL_INTERVAL":       "GOTCHA_SLO_EVAL_INTERVAL_SECONDS",
	"GOTCHA_ESCALATION_INTERVAL":     "GOTCHA_ESCALATION_INTERVAL_SECONDS",
	"GOTCHA_RETENTION_DAYS":          "GOTCHA_EVENT_RETENTION_DAYS",
	"GOTCHA_SERVER_URL":              "GOTCHA_PROBE_SERVER_URL",
	"GOTCHA_INGEST_RATE_LIMIT":       "GOTCHA_INGEST_RATE_PER_SEC",
	"GOTCHA_AGENT_DIST_DIR":          "GOTCHA_DIST_DIR",
	"GOTCHA_AGENT_DIST_RATE_PER_MIN": "GOTCHA_DIST_RATE_PER_MIN",
}

// gotchaTokenRe находит МАКСИМАЛЬНЫЙ идентификатор вида GOTCHA_..., а не
// голое вхождение старого имени как подстроки. Границы \b по обе стороны и
// жадный [A-Z0-9_]* до конца токена гарантируют:
//   - GOTCHA_RETENTION_DAYS не поймает GOTCHA_LOG_RETENTION_DAYS,
//     GOTCHA_SPAN_RETENTION_DAYS и ещё пять законных переменных с тем же
//     суффиксом — они разбираются в ДРУГОЙ, более длинный токен целиком,
//     который со старым именем побайтово не совпадает;
//   - GOTCHA_METRIC_EVAL_INTERVAL (и ещё четыре переменные того же
//     семейства *_EVAL_INTERVAL/*_INTERVAL) не поймает уже переименованный
//     GOTCHA_METRIC_EVAL_INTERVAL_SECONDS —
//     жадный матч утянет суффикс "_SECONDS" в тот же токен целиком.
//
// Сравнение идёт по РАВЕНСТВУ токена со старым именем (см. цикл ниже), а не
// по regexp.MatchString на каждое старое имя по отдельности — иначе тот же
// вопрос о границе слева/справа пришлось бы решать десять раз, один раз на
// каждое имя, вместо одного разбора строки на токены.
var gotchaTokenRe = regexp.MustCompile(`\bGOTCHA_[A-Z0-9_]*\b`)

// renamedEnvVarsExtensions — файлы, которые сторож читает как текст: код,
// шаблоны, конфиги и документация. Расширения, а не попытка прочитать любой
// файл дерева, — так собранные бинарники в корне репозитория (gotcha,
// gotcha-agent, devseed) и прочие не-текстовые файлы не открываются вовсе.
var renamedEnvVarsExtensions = map[string]bool{
	".go":    true,
	".templ": true,
	".md":    true,
	".sql":   true,
	".yml":   true,
	".yaml":  true,
	".sh":    true,
}

// renamedEnvVarsExactNames — файлы без содержательного расширения, но
// целиком по имени: .env.example — единственный полный справочник
// переменных окружения в репозитории (см. env_example_test.go), Dockerfile
// и Makefile передают переменные в сборку/образ и тоже могли бы застрять со
// старым именем.
var renamedEnvVarsExactNames = map[string]bool{
	".env.example": true,
	"Dockerfile":   true,
	"Makefile":     true,
}

// renamedEnvVarsSkipRootDirs — каталоги корня, не относящиеся к
// git-версионируемым исходникам и документации проекта: сверены с
// .gitignore. vendor — сторонний вендоренный код (та же причина, что и в
// tree.go); docs (там же лежит docs/tech-docs) — «Local planning & docs (not
// part of the published repo)» согласно .gitignore, gitignore-запись /docs;
// cld — гитигнорный рабочий каталог спек/планов/команд (CLAUDE.md,
// «Спеки/планы/команды — в гитигнорном cld/»); .superpowers,
// .playwright-mcp, .remember — служебные кэши инструментария агента
// (сессионные дампы браузера, память между сессиями) — тоже в .gitignore, и
// первый прогон этого сторожа показал ПОЧЕМУ их обязательно нужно
// исключать: они годами копят неактуальные скриншоты/заметки со старыми
// именами переменных, которые никогда не были частью опубликованного
// репозитория и никто их не поддерживал в актуальном состоянии; deploy —
// инфраструктурные XML-конфиги ClickHouse без переменных окружения
// продукта. Сверяются с ОТНОСИТЕЛЬНЫМ ПУТЁМ целиком (тем же приёмом, что и
// tree.go), потому что это каталоги именно корня, а не имена, которые могли
// бы повториться где-то глубже с другим смыслом.
var renamedEnvVarsSkipRootDirs = map[string]bool{
	"vendor":          true,
	"docs":            true,
	"cld":             true,
	".superpowers":    true,
	".playwright-mcp": true,
	".remember":       true,
	"deploy":          true,
}

// TestNoRenamedEnvVarNames — ни одно из десяти старых имён переменных
// окружения (renamedEnvVars) не встречается нигде в дереве исходников и
// документации. Мотивация — реальная находка ЧЕЛОВЕКА, а не гейта:
// internal/web/probes.go рисовал пользователю команду
// `docker run -e GOTCHA_SERVER_URL=…`, нерабочую после переименования этой
// переменной; скопированная в терминал команда молча ничего не делала.
//
// Область — весь текстовый код и документация проекта, КРОМЕ:
//   - vendor/, .git/, node_modules — сторонний код, не наш;
//   - internal/guards/ целиком (а не только этот файл) — тот же приём,
//     которым уже пользуются другие построчные сторожа пакета
//     (format_test.go, migrations_test.go, flash_test.go,
//     selfmetrics_docs_test.go: strings.HasPrefix(path, "internal/guards/")):
//     фикстуры соседних сторожей по определению держат «плохие» по чужим
//     правилам образцы. Конкретно здесь — env_example_test.go:298 держит
//     голое старое имя GOTCHA_ESCALATION_INTERVAL как отрицательный
//     тест-кейс конвенции единиц измерения (TestUnitSuffixConvention:
//     ожидается hasUnitSuffix == false для срока БЕЗ единицы в имени).
//     Исключение точечно одного файла сторожа не закрыло бы эту находку и
//     потребовало бы отдельного списка; исключение всего пакета — уже
//     проверенный в этом репозитории приём, который заодно исключает и сам
//     этот файл (в нём все десять старых имён перечислены по определению,
//     см. renamedEnvVars) без отдельной строки самоисключения, которая
//     сама могла бы стать дырой;
//   - CHANGELOG.md/CHANGELOG.ru.md — там старые имена верны как
//     историческая запись прошлых релизов, править их значило бы
//     фальсифицировать историю.
//
// Обход собственный, не через guards.Tree/Load: Tree (tree.go) намеренно не
// включает markdown-документацию и .env.example — как раз те файлы, ради
// которых заведён именно этот сторож, а не только *.go/*.templ. Обход
// ограничен расширениями (renamedEnvVarsExtensions/renamedEnvVarsExactNames)
// и пропускает всё остальное без чтения — та же по порядку величины цена,
// что и обход в tree.go (там же — «обход стоит десятки миллисекунд»), и
// делается синхронно один раз за вызов теста; повторных обходов дерева на
// каждый пакет тестов, обёрнутый в этот же процесс `go test`, не возникает,
// потому что go test одного пакета запускает этот тест ровно один раз.
func TestNoRenamedEnvVarNames(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatalf("findRoot: %v", err)
	}

	type findingT struct {
		path string
		line int
		old  string
	}
	var findings []findingT

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if renamedEnvVarsSkipRootDirs[rel] || skipAnyDepthDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasPrefix(rel, "internal/guards/") {
			return nil
		}
		if rel == "CHANGELOG.md" || rel == "CHANGELOG.ru.md" {
			return nil
		}
		if !renamedEnvVarsExtensions[filepath.Ext(rel)] && !renamedEnvVarsExactNames[filepath.Base(rel)] {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			for _, tok := range gotchaTokenRe.FindAllString(line, -1) {
				if _, bad := renamedEnvVars[tok]; bad {
					findings = append(findings, findingT{path: rel, line: i + 1, old: tok})
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("обход дерева: %v", walkErr)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		t.Errorf("%s:%d: встречается удалённое имя переменной окружения %s — замените на %s",
			f.path, f.line, f.old, renamedEnvVars[f.old])
	}
}
