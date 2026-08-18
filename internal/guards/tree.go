// Package guards содержит сторожевые тесты, перебирающие дерево исходников
// репозитория с явным списком исключений вместо перечисления проверяемого
// множества вручную. Причина существования пакета: сторожа, перечислявшие
// проверяемое множество руками, расходились между собой и пропускали
// находки — правки проходили гейт зелёными, хотя нарушали правило.
//
// Перед тем как писать следующее построчное правило, проверить два пункта:
//
//  1. Вычистка "//"-комментариев с защитой от URL в строковом литерале —
//     обязательный первый шаг любого построчного сканера. Приём готов, это
//     stripTrailingComment (i18n_leak_test.go) — переиспользовать буквально,
//     не изобретать заново.
//  2. Правило, которое ищет паттерны КОДА (не разметки, не стилей, не
//     ключей каталога), обязано исключать из обхода файлы самого пакета
//     internal/guards — иначе его же тестовые фикстуры и примеры в
//     комментариях обманут правило.
package guards

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// File — один прочитанный файл дерева: путь относительно корня репозитория
// (пригодный для печати в сообщениях сторожей), содержимое и признак
// «сгенерирован», чтобы правила про авторский код могли его игнорировать.
type File struct {
	Path      string
	Body      string
	Generated bool
}

// Tree — единый снимок дерева исходников, на котором работают все сторожа
// пакета guards. Собирается один раз на пакет: правил девять, а обход стоит
// десятки миллисекунд — дублировать его в каждом тесте незачем.
type Tree struct {
	Root         string
	GoFiles      []File
	Templates    []File
	CSS          File
	Catalogs     map[string]map[string]string
	Plurals      map[string]map[string]map[string]string
	MigrationsPG []File
	MigrationsCH []File
}

// skipRootDirs — каталоги, которые существуют только в корне репозитория и
// не относятся к продуктовому коду: сверяются с ОТНОСИТЕЛЬНЫМ ПУТЁМ целиком,
// а не с именем каталога. Раньше сверялись с d.Name(), и одноимённый
// internal/docs (реальный пакет продукта) выпадал из обхода вместе с
// корневым docs (markdown-документация) — обе имели имя "docs".
var skipRootDirs = map[string]bool{
	"docs":         true,
	"deploy":       true,
	"cld":          true,
	".superpowers": true,
	// vendor — вендоренные Go-зависимости (появились с офлайн-сборкой v0.6.2):
	// сторонний код, к нашим правилам форматирования/стиля отношения не имеет
	// (тот же класс, что node_modules ниже). Без пропуска сторожа обхода дерева
	// (напр. TestNoRawTimeFormattingOutsideHumanize) падают на .Format() внутри
	// pgx/protobuf/logrus и т.п.
	"vendor": true,
}

// skipAnyDepthDirs — каталоги, которые пропускаются на любой глубине: .git
// может встретиться в сабмодулях, node_modules — в вендоренных фронтенд-
// зависимостях где угодно в дереве. Сверяются с именем каталога, потому что
// продуктового кода с такими именами по определению быть не может.
var skipAnyDepthDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
}

var (
	loadOnce   sync.Once
	loadedTree *Tree
	loadErr    error
)

// Load возвращает снимок дерева исходников репозитория, собирая его при
// первом вызове в рамках пакета. При ошибке проваливает тест через
// t.Fatalf — сторожам, использующим Tree, отдельно проверять ошибку не
// нужно.
func Load(t *testing.T) *Tree {
	t.Helper()
	loadOnce.Do(func() {
		loadedTree, loadErr = loadTree()
	})
	if loadErr != nil {
		t.Fatalf("guards.Load: %v", loadErr)
	}
	return loadedTree
}

func loadTree() (*Tree, error) {
	root, err := findRoot()
	if err != nil {
		return nil, err
	}

	tree := &Tree{
		Root:     root,
		Catalogs: map[string]map[string]string{},
		Plurals:  map[string]map[string]map[string]string{},
	}
	pgDir := filepath.Join("internal", "db", "migrations", "pg") + string(filepath.Separator)
	chDir := filepath.Join("internal", "db", "migrations", "ch") + string(filepath.Separator)
	cssPath := filepath.Join("internal", "web", "static", "app.css")

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
		if d.IsDir() {
			if skipRootDirs[rel] || skipAnyDepthDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		switch {
		case strings.HasSuffix(path, ".go"):
			f, err := readFile(root, rel)
			if err != nil {
				return err
			}
			f.Generated = isGenerated(f)
			tree.GoFiles = append(tree.GoFiles, f)
		case strings.HasSuffix(path, ".templ"):
			f, err := readFile(root, rel)
			if err != nil {
				return err
			}
			tree.Templates = append(tree.Templates, f)
		case rel == cssPath:
			f, err := readFile(root, rel)
			if err != nil {
				return err
			}
			tree.CSS = f
		case strings.HasSuffix(path, ".sql") && strings.HasPrefix(rel, pgDir):
			f, err := readFile(root, rel)
			if err != nil {
				return err
			}
			tree.MigrationsPG = append(tree.MigrationsPG, f)
		case strings.HasSuffix(path, ".sql") && strings.HasPrefix(rel, chDir):
			f, err := readFile(root, rel)
			if err != nil {
				return err
			}
			tree.MigrationsCH = append(tree.MigrationsCH, f)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	for _, loc := range []string{"ru", "en"} {
		messages, plurals, err := loadLocale(filepath.Join(root, "internal", "i18n", "locales", loc+".json"))
		if err != nil {
			return nil, err
		}
		tree.Catalogs[loc] = messages
		tree.Plurals[loc] = plurals
	}

	return tree, nil
}

// findRoot ищет корень репозитория вверх от рабочей директории по наличию
// go.mod. Тесты запускаются из директории своего пакета — без этого поиска
// относительные пути в Tree были бы привязаны к internal/guards, а не к
// корню.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod не найден ни в одном из родительских каталогов от %s", dir)
		}
		dir = parent
	}
}

func readFile(root, rel string) (File, error) {
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return File{}, err
	}
	return File{Path: filepath.ToSlash(rel), Body: string(body)}, nil
}

// isGenerated отличает сгенерированный код от авторского: по суффиксу
// _templ.go (templ всегда его использует) и по маркеру "Code generated" в
// первых трёх строках — так же его ищет `go generate` и golang.org/x/tools.
func isGenerated(f File) bool {
	if strings.HasSuffix(f.Path, "_templ.go") {
		return true
	}
	lines := strings.SplitN(f.Body, "\n", 4)
	for i := 0; i < len(lines) && i < 3; i++ {
		if strings.Contains(lines[i], "Code generated") {
			return true
		}
	}
	return false
}

// localeFile — форма JSON-файла каталога локали: messages — обычные строки
// «ключ → значение», plurals — формы множественного числа «ключ → форма →
// значение». Каталог не плоский, вопреки первому предположению — проверено
// по internal/i18n/locales/ru.json перед написанием разбора.
type localeFile struct {
	Messages map[string]string            `json:"messages"`
	Plurals  map[string]map[string]string `json:"plurals"`
}

// loadLocale читает каталог локали и возвращает messages и plurals по
// отдельности, не сливая их в общий плоский набор.
//
// Раньше формы plurals разворачивались в тот же плоский набор ключами вида
// "<ключ>.<форма>" — это ломается на совпадении: в каталоге уже есть обычный
// message-ключ platform.other, неотличимый по такой схеме от гипотетической
// формы "other" плюрального ключа platform.
// Раздельные поля дают правилам однозначный ответ «этот ключ обычный или
// плюральный», не полагаясь на угадывание по суффиксу.
func loadLocale(path string) (messages map[string]string, plurals map[string]map[string]string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var lf localeFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return lf.Messages, lf.Plurals, nil
}
