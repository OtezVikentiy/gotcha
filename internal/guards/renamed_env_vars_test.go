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

var renamedEnvVars = envcontract.Renamed

// \b + жадный [A-Z0-9_]* до конца токена — не даёт короткому старому имени совпасть
// внутри длинного легитимного (GOTCHA_METRIC_EVAL_INTERVAL vs ..._SECONDS).
var gotchaTokenRe = regexp.MustCompile(`\bGOTCHA_[A-Z0-9_]*\b`)

var renamedEnvVarsExtensions = map[string]bool{
	".go":    true,
	".templ": true,
	".md":    true,
	".sql":   true,
	".yml":   true,
	".yaml":  true,
	".sh":    true,
}

var renamedEnvVarsExactNames = map[string]bool{
	".env.example": true,
	"Dockerfile":   true,
	"Makefile":     true,
}

var renamedEnvVarsSkipRootDirs = map[string]bool{
	"vendor":          true,
	"docs":            true,
	"cld":             true,
	".superpowers":    true,
	".playwright-mcp": true,
	".remember":       true,
	"deploy":          true,
}

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
		if rel == "internal/docs/ru/upgrade.md" || rel == "internal/docs/en/upgrade.md" {
			return nil
		}
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

func allRenamedOldNames() []string {
	names := make([]string, 0, len(envcontract.Renamed))
	for old := range envcontract.Renamed {
		names = append(names, old)
	}
	sort.Strings(names)
	return names
}

func TestUpgradeDocDocumentsAllRenamedPairs(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatalf("findRoot: %v", err)
	}
	names := allRenamedOldNames()
	if len(names) < 40 {
		t.Fatalf("allRenamedOldNames() вернула %d имён, ожидалось ≥40 (10 v0.23.0 + 17 серверных + 3 агентских + 11 compose/build) — envcontract.Renamed урезан или обход сломан", len(names))
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
				t.Fatalf("envcontract.Renamed[%s] пуст — allRenamedOldNames() содержит имя вне реестра", old)
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
