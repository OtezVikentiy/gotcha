package guards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type codeVersions struct {
	goVersion string
	pgVersion string
	chVersion string
}

var (
	goModVersionRe     = regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)`)
	dockerGoVersionRe  = regexp.MustCompile(`golang:(\d+\.\d+(?:\.\d+)?)-alpine@sha256:`)
	composePGVersionRe = regexp.MustCompile(`postgres:(\d+)-alpine@sha256:`)
	composeCHVersionRe = regexp.MustCompile(`clickhouse-server:(\d+\.\d+)-alpine@sha256:`)
)

var versionDigitsRe = regexp.MustCompile(`\d+(?:\.\d+)*`)

// Принимает как голое число ("17"), так и фразу с текстом вокруг ("Go 1.26+");
// в обоих случаях остаются первые два компонента — патч и суффикс не участвуют в сверке.
func normalizeVersion(v string) string {
	digits := versionDigitsRe.FindString(v)
	parts := strings.Split(digits, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

func readDocFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	return string(body)
}

func extractVersion(t *testing.T, re *regexp.Regexp, body, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s: версия не найдена — сторож смотрит мимо файла", what)
	}
	return normalizeVersion(m[1])
}

func loadCodeVersions(t *testing.T, root string) codeVersions {
	t.Helper()
	goMod := readDocFile(t, filepath.Join(root, "go.mod"))
	dockerfile := readDocFile(t, filepath.Join(root, "Dockerfile"))
	compose := readDocFile(t, filepath.Join(root, "docker-compose.yml"))

	goFromMod := extractVersion(t, goModVersionRe, goMod, "go.mod")
	goFromDocker := extractVersion(t, dockerGoVersionRe, dockerfile, "Dockerfile")
	if goFromMod != goFromDocker {
		t.Errorf("go.mod держит Go %s, Dockerfile — Go %s: истины разошлись между собой", goFromMod, goFromDocker)
	}

	return codeVersions{
		goVersion: goFromMod,
		pgVersion: extractVersion(t, composePGVersionRe, compose, "docker-compose.yml (postgres)"),
		chVersion: extractVersion(t, composeCHVersionRe, compose, "docker-compose.yml (clickhouse)"),
	}
}

type docTarget struct {
	label  string
	path   string
	locale string
}

// README.md/README.ru.md — пара переводов, как CHANGELOG: у README нет
// локаль-нейтрального варианта, английский файл сам является локалью en.
func docVersionTargets(root string) []docTarget {
	return []docTarget{
		{"README.md", filepath.Join(root, "README.md"), "en"},
		{"README.ru.md", filepath.Join(root, "README.ru.md"), "ru"},
		{"internal/docs/en/installation.md", filepath.Join(root, "internal", "docs", "en", "installation.md"), "en"},
		{"internal/docs/ru/installation.md", filepath.Join(root, "internal", "docs", "ru", "installation.md"), "ru"},
	}
}

type versionSpec struct {
	system string
	re     *regexp.Regexp
	code   func(codeVersions) string
}

var versionSpecs = []versionSpec{
	{"Go", regexp.MustCompile(`Go (\d+\.\d+(?:\.\d+)?)\+?`), func(c codeVersions) string { return c.goVersion }},
	{"PostgreSQL", regexp.MustCompile(`PostgreSQL (\d+)`), func(c codeVersions) string { return c.pgVersion }},
	{"ClickHouse", regexp.MustCompile(`ClickHouse (\d+\.\d+)`), func(c codeVersions) string { return c.chVersion }},
}

type requiredMention struct {
	system string
	locale string
}

// Список зафиксирован по факту сканирования доков, а не выводится программно:
// каждая из трёх систем названа хотя бы раз в каждой локали.
var requiredVersionMentions = []requiredMention{
	{"Go", "en"},
	{"Go", "ru"},
	{"PostgreSQL", "en"},
	{"PostgreSQL", "ru"},
	{"ClickHouse", "en"},
	{"ClickHouse", "ru"},
}

func TestDocVersionsMatchCode(t *testing.T) {
	tree := Load(t)
	code := loadCodeVersions(t, tree.Root)

	mentioned := map[requiredMention]bool{}
	for _, target := range docVersionTargets(tree.Root) {
		body := readDocFile(t, target.path)
		for _, spec := range versionSpecs {
			matches := spec.re.FindAllStringSubmatch(body, -1)
			if len(matches) == 0 {
				continue
			}
			mentioned[requiredMention{spec.system, target.locale}] = true
			want := spec.code(code)
			for _, m := range matches {
				got := normalizeVersion(m[1])
				if got != want {
					t.Errorf("%s: %s упомянут как %s, в коде %s", target.label, spec.system, got, want)
				}
			}
		}
	}

	for _, req := range requiredVersionMentions {
		if !mentioned[req] {
			t.Errorf("%s (%s): версия не найдена ни в одном доке этой локали, хотя по замеру должна упоминаться", req.system, req.locale)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1.26.6", "1.26"},
		{"Go 1.26+", "1.26"},
		{"17", "17"},
		{"25.3", "25.3"},
	}
	for _, c := range cases {
		if got := normalizeVersion(c.in); got != c.want {
			t.Errorf("normalizeVersion(%q) = %q, хотим %q", c.in, got, c.want)
		}
	}
}
