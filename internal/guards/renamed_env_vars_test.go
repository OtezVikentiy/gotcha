package guards

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
)

// renamedEnvVars — старые имена переменных окружения (переименованы этой
// волной, cmd/gotcha/config.go и internal/agent/config.go их больше не
// читают) → новые. Переименование уже один раз оставило за собой настоящий
// баг: internal/web/probes.go рисовал пользователю в интерфейсе команду
// `docker run -e GOTCHA_SERVER_URL=…`, которой после переименования не
// существовало — скопированная команда молча не работала бы, и поймал это
// человек, а не гейт. Этот сторож закрывает класс находки целиком: ни одно
// из старых имён (все волны переименования) не должно встречаться нигде в дереве (кроме
// CHANGELOG и файла-истины — см. докблок TestNoRenamedEnvVarNames).
//
// Сам список — не собственная копия, а псевдоним envcontract.Renamed
// (internal/envcontract/renamed.go, ЕДИНСТВЕННАЯ истина). Держать здесь
// вторую копию значило бы завести ровно ту дыру, ради закрытия которой этот
// сторож и написан: при следующем переименовании кто-то поправил бы карту в
// cmd/gotcha/config.go, забыл про копию сторожа — и сторож продолжал бы
// зелено проверять уже неактуальный список.
var renamedEnvVars = envcontract.Renamed

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

// TestNoRenamedEnvVarNames — ни одно из старых имён переменных (все волны переименования)
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
//     этот файл (renamedEnvVars — псевдоним envcontract.Renamed, а его
//     идентификаторы-ключи карты сами являются токенами GOTCHA_..., пусть и
//     не буквальным литералом) без отдельной строки самоисключения, которая
//     сама могла бы стать дырой;
//   - internal/envcontract/renamed.go — файл-истина (см. renamedEnvVars
//     выше). Исключение ТОЧЕЧНО одного файла, а не всего пакета: envcontract
//     может обрасти другими файлами (тестом карты, будущими фактами о
//     совместимости env), и они не должны получить безусловный пропуск
//     старых имён просто по соседству с истиной;
//   - CHANGELOG.md/CHANGELOG.ru.md — там старые имена верны как
//     историческая запись прошлых релизов, править их значило бы
//     фальсифицировать историю;
//   - internal/docs/ru/upgrade.md и internal/docs/en/upgrade.md (задача 11,
//     круг правок) — страница апгрейда обязана называть старое имя
//     буквально, парами «было → стало»: оператору нужен список для sed по
//     собственному .env, отсылка «см. CHANGELOG» этого не даёт. Полноту
//     этих таблиц (что каждая пара текущей волны переименования реально
//     присутствует в upgrade.md обеих локалей) проверяет отдельный сторож,
//     TestUpgradeDocDocumentsCurrentRenameWave — исключение отсюда не
//     превращается в дыру;
//   - cmd/gotcha/renamed_env_contract_test.go — держит независимую сверку
//     envcontract.Renamed с документированным в CHANGELOG списком
//     (TestEnvcontractRenamedComplete): её want-таблица по определению
//     повторяет все старые имена буквально, иначе тест сверял бы карту
//     саму с собой и не заметил бы порчи. Исключение точечно ЭТОГО файла, а
//     не всего cmd/gotcha: config_test.go (где живут поведенческие тесты
//     самого отказа старта) старое имя не пишет НИ РАЗУ — тесты берут пару
//     прямо из envcontract.Renamed (sortedRenamedOldNames), поэтому остаётся
//     под сторожем целиком и растёт вместе с фичами конфига, не становясь
//     слепой зоной.
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
		// upgrade.md обеих локалей (задача 11, круг правок): страница
		// апгрейда обязана называть старое имя буквально — оператору нужен
		// список «было → стало» для sed по собственному .env, а не абстрактная
		// отсылка «см. CHANGELOG». Тот же случай, что и у CHANGELOG.{md,ru.md}
		// выше: старое имя здесь — не забытый огрызок контракта, а сама суть
		// страницы. Полнота этих таблиц (что каждая пара текущей волны
		// переименования — AgentOwned/InfraOwned/ServerOwned из
		// envcontract — реально присутствует в upgrade.md обеих локалей)
		// проверяется отдельно, TestUpgradeDocDocumentsCurrentRenameWave
		// ниже — исключение здесь не превращается в дыру, потому что полноту
		// таблиц ловит другой сторож.
		if rel == "internal/docs/ru/upgrade.md" || rel == "internal/docs/en/upgrade.md" {
			return nil
		}
		// internal/envcontract/renamed.go — файл-истина,
		// cmd/gotcha/renamed_env_contract_test.go — справочник для сверки
		// с CHANGELOG; оба исключения разобраны в докблоке
		// TestNoRenamedEnvVarNames выше. cmd/gotcha/config_test.go под
		// сторожем целиком — его поведенческие тесты старое имя не пишут.
		if rel == "internal/envcontract/renamed.go" || rel == "cmd/gotcha/renamed_env_contract_test.go" {
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

// currentRenameWaveOldNames — старые имена ТЕКУЩЕЙ волны переименования (E3,
// заморозка контракта перед 1.0), которую документирует upgrade.md буквальными
// таблицами «было → стало»: объединение envcontract.AgentOwned, ServerOwned и
// InfraOwned. Более ранняя волна v0.23.0 (десять переменных) сюда намеренно
// не входит — она уже вышла отдельным релизом, её таблица «было → стало»
// остаётся исторической записью под своей версией в CHANGELOG, а не текущей
// инструкцией по апгрейду (см. докблок ServerOwned в
// internal/envcontract/renamed.go).
func currentRenameWaveOldNames() []string {
	names := make([]string, 0, len(envcontract.AgentOwned)+len(envcontract.ServerOwned)+len(envcontract.InfraOwned))
	names = append(names, envcontract.AgentOwned...)
	names = append(names, envcontract.ServerOwned...)
	names = append(names, envcontract.InfraOwned...)
	return names
}

// TestUpgradeDocDocumentsCurrentRenameWave — задача 11, круг правок, п.2:
// каждая пара «было → стало» из currentRenameWaveOldNames() обязана
// буквально присутствовать в upgrade.md ОБЕИХ локалей — иначе исключение
// этих файлов из TestNoRenamedEnvVarNames (см. докблок выше) стало бы
// дырой: старое имя разрешено писать в upgrade.md, но НЕ проверяется, что
// оно там реально есть.
//
// Мутация, которую держит в уме ревьюер задачи: убрать сверку по одной из
// локалей — красит именно РАЗДЕЛЬНЫЙ цикл по ru/en ниже, не объединённое
// множество имён.
func TestUpgradeDocDocumentsCurrentRenameWave(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatalf("findRoot: %v", err)
	}
	names := currentRenameWaveOldNames()
	if len(names) < 20 {
		t.Fatalf("currentRenameWaveOldNames() вернула %d имён, ожидалось ≥20 (17 серверных + 3 агентских + 11 compose/build) — AgentOwned/ServerOwned/InfraOwned урезаны или обход сломан", len(names))
	}

	for _, loc := range []string{"ru", "en"} {
		path := filepath.Join(root, "internal", "docs", loc, "upgrade.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		text := string(body)
		for _, old := range names {
			newName := envcontract.Renamed[old]
			if newName == "" {
				t.Fatalf("envcontract.Renamed[%s] пуст — currentRenameWaveOldNames() содержит имя вне реестра", old)
			}
			if !strings.Contains(text, old) {
				t.Errorf("%s: upgrade.md не содержит старое имя %s (пара %s → %s)", loc, old, old, newName)
			}
			if !strings.Contains(text, newName) {
				t.Errorf("%s: upgrade.md не содержит новое имя %s (пара %s → %s)", loc, newName, old, newName)
			}
		}
	}
}
