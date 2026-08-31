package guards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// changelogHeadings — канонический словарь ###-заголовков по файлам: шесть
// заголовков Keep a Changelog плюс принятые в репозитории расширения.
// Скобочные варианты («Fixed (usability)») запрещены: тематика пункта
// выражается первой фразой самого пункта, а не заголовком секции.
var changelogHeadings = map[string][]string{
	"CHANGELOG.md": {
		"Added", "Changed", "Deprecated", "Removed", "Fixed", "Security", "Breaking",
		"Performance", "Accessibility", "Interface", "Documentation", "Testing",
	},
	"CHANGELOG.ru.md": {
		"Добавлено", "Изменено", "Устарело", "Удалено", "Исправлено", "Безопасность", "Ломающее",
		"Производительность", "Доступность", "Интерфейс", "Документация", "Тестирование",
	},
}

// TestChangelogSectionHeadings — №128: внутри КАЖДОЙ версии-секции (## […])
// обоих changelog-файлов ###-заголовки берутся из словаря и не повторяются.
// Один заголовок — одна секция: дубли (Fixed ×4) заставляли читателя собирать
// список исправлений по всему файлу.
func TestChangelogSectionHeadings(t *testing.T) {
	tree := Load(t)
	for file, allowed := range changelogHeadings {
		allowedSet := map[string]bool{}
		for _, h := range allowed {
			allowedSet[h] = true
		}
		body, err := os.ReadFile(filepath.Join(tree.Root, file))
		if err != nil {
			t.Fatal(err)
		}
		section := "(preamble)"
		seen := map[string]bool{}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "## ") {
				section = strings.TrimSpace(strings.TrimPrefix(line, "## "))
				seen = map[string]bool{}
				continue
			}
			if !strings.HasPrefix(line, "### ") {
				continue
			}
			h := strings.TrimSpace(strings.TrimPrefix(line, "### "))
			if !allowedSet[h] {
				t.Errorf("%s:%d: секция %s: заголовок %q вне словаря", file, i+1, section, h)
				continue
			}
			if seen[h] {
				t.Errorf("%s:%d: секция %s: заголовок %q повторяется", file, i+1, section, h)
			}
			seen[h] = true
		}
	}
}
