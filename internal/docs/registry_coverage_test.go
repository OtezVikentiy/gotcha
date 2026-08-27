package docs

import (
	"sort"
	"strings"
	"testing"
)

// mdSlugsOnDisk перечисляет slug'и (имя файла без ".md") всех markdown-файлов
// локали в embed.FS — то, что реально лежит на диске, а не то, что знает
// registry.
func mdSlugsOnDisk(t *testing.T, loc string) map[string]bool {
	t.Helper()
	entries, err := files.ReadDir(loc)
	if err != nil {
		t.Fatalf("files.ReadDir(%q): %v", loc, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = true
	}
	return out
}

// registrySlugs — все slug'и, перечисленные в registry (порядок оглавления).
func registrySlugs() map[string]bool {
	out := make(map[string]bool, len(registry))
	for _, r := range registry {
		out[r.Slug] = true
	}
	return out
}

// TestEveryMarkdownFileIsInRegistry ловит именно тот класс бага, из-за
// которого эта проверка появилась: markdown-файл страницы лежит в
// internal/docs/{ru,en}/, но забыт в registry — Render()/Pages() его не
// отдают, все ссылки на /docs/<slug> внутри других страниц ведут на 404, а
// TestPagesRegistryBothLocales (сверяющий только len(Pages(loc)) ==
// len(registry)) эту дыру не видит: он не смотрит на диск вообще, только на
// сам registry. Проверка идёт по ИМЕНАМ файлов, а не по номеру строки —
// добавление/переименование страницы ловится независимо от того, куда в
// registry её вписали.
func TestEveryMarkdownFileIsInRegistry(t *testing.T) {
	want := registrySlugs()
	for _, loc := range []string{"ru", "en"} {
		onDisk := mdSlugsOnDisk(t, loc)

		var missingFromRegistry []string
		for slug := range onDisk {
			if !want[slug] {
				missingFromRegistry = append(missingFromRegistry, slug)
			}
		}
		sort.Strings(missingFromRegistry)
		for _, slug := range missingFromRegistry {
			t.Errorf("internal/docs/%s/%s.md существует на диске, но не упомянут в registry (internal/docs/docs.go) — страница недостижима через /docs/%s", loc, slug, slug)
		}

		var missingFile []string
		for slug := range want {
			if !onDisk[slug] {
				missingFile = append(missingFile, slug)
			}
		}
		sort.Strings(missingFile)
		for _, slug := range missingFile {
			t.Errorf("registry (internal/docs/docs.go) упоминает %q, но internal/docs/%s/%s.md не существует", slug, loc, slug)
		}
	}
}
