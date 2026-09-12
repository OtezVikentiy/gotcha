package docs

import (
	"sort"
	"strings"
	"testing"
)

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

func registrySlugs() map[string]bool {
	out := make(map[string]bool, len(registry))
	for _, r := range registry {
		out[r.Slug] = true
	}
	return out
}

// TestPagesRegistryBothLocales сверяет только len(Pages(loc)) == len(registry) —
// файл, забытый в registry, эту дыру не ловит, а страница по /docs/<slug> даст 404.
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
