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

type File struct {
	Path      string
	Body      string
	Generated bool
}

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

// сверяются с относительным путём целиком, не с именем каталога: одноимённый
// internal/docs — рабочий пакет продукта, а не то, что нужно пропускать.
var skipRootDirs = map[string]bool{
	"docs":         true,
	"deploy":       true,
	"cld":          true,
	".superpowers": true,
	"vendor":       true,
}

// пропускаются на любой глубине, сверка по имени каталога.
var skipAnyDepthDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
}

var (
	loadOnce   sync.Once
	loadedTree *Tree
	loadErr    error
)

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

type localeFile struct {
	Messages map[string]string            `json:"messages"`
	Plurals  map[string]map[string]string `json:"plurals"`
}

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
