package guards

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

type ingestPatternRecorder struct{ patterns []string }

func (r *ingestPatternRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

type ingestRoutePattern struct {
	method string
	path   string
}

// "{$}" в конце паттерна значим для ServeMux, но адресом не является:
// /api/{project}/store/{$} и документируемый /api/7/store/ — один адрес.
func normalizeRoutePath(p string) string {
	return strings.TrimSuffix(p, "{$}")
}

func registeredIngestRoutes(t *testing.T) []ingestRoutePattern {
	t.Helper()
	rec := &ingestPatternRecorder{}
	(&ingest.Handler{}).Register(rec)
	if len(rec.patterns) == 0 {
		t.Fatal("записано 0 паттернов приёма — сторож смотрит мимо регистрации")
	}
	out := make([]ingestRoutePattern, 0, len(rec.patterns))
	for _, p := range rec.patterns {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			t.Fatalf("паттерн %q без метода", p)
		}
		out = append(out, ingestRoutePattern{method: method, path: normalizeRoutePath(path)})
	}
	return out
}

// Кандидаты берём только из машинной разметки markdown: свободная проза даёт
// обрывки и адреса-примеры чужих сервисов. Класс символов включает `<>?#` —
// плейсхолдеры вида `<PROJECT_ID>` и query-строку нужно захватить целиком,
// чтобы обрезать и нормализовать их, а не потерять адрес на полпути.
var (
	fencedBlockRe    = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRe     = regexp.MustCompile("`[^`\n]+`")
	ingestPathRe     = regexp.MustCompile(`/(?:api|v1)/[A-Za-z0-9_{}./<>?#-]*`)
	numericSegRe     = regexp.MustCompile(`/[0-9]+(/|$)`)
	placeholderSegRe = regexp.MustCompile(`/<[^/>]*>(/|$)`)
)

func normalizeDocPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = placeholderSegRe.ReplaceAllString(p, "/{project}$1")
	return numericSegRe.ReplaceAllString(p, "/{project}$1")
}

func docPathCandidates(t *testing.T, root, lang string) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "internal", "docs", lang, "*.md"))
	if err != nil {
		t.Fatalf("glob доков %s: %v", lang, err)
	}
	if len(paths) == 0 {
		t.Fatalf("в internal/docs/%s не найдено ни одного .md — сторож смотрит мимо доков", lang)
	}
	out := map[string]bool{}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("чтение %s: %v", p, err)
		}
		src := string(body)
		for _, chunk := range append(fencedBlockRe.FindAllString(src, -1), inlineCodeRe.FindAllString(src, -1)...) {
			for _, m := range ingestPathRe.FindAllString(chunk, -1) {
				out[normalizeDocPath(m)] = true
			}
		}
	}
	return out
}

const minDocPathCandidates = 12 // 16 кандидатов в доках сейчас; запас вниз против дрожания

// Кандидат, который является строгим префиксом зарегистрированного адреса и
// сам оканчивается слешем, — упоминание неймспейса (/api/v1/), а не эндпоинт.
func isNamespaceMention(candidate string, registered map[string]bool) bool {
	if !strings.HasSuffix(candidate, "/") {
		return false
	}
	for r := range registered {
		if r != candidate && strings.HasPrefix(r, candidate) {
			return true
		}
	}
	return false
}

func TestDocIngestPathsAreRegistered(t *testing.T) {
	tree := Load(t)
	registered := map[string]bool{}
	for _, r := range registeredIngestRoutes(t) {
		registered[r.path] = true
	}

	total := 0
	for _, lang := range []string{"ru", "en"} {
		candidates := docPathCandidates(t, tree.Root, lang)
		total += len(candidates)
		keys := make([]string, 0, len(candidates))
		for k := range candidates {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, c := range keys {
			if registered[c] || isNamespaceMention(c, registered) {
				continue
			}
			t.Errorf("internal/docs/%s: путь %q не зарегистрирован среди маршрутов приёма", lang, c)
		}
	}
	if total < minDocPathCandidates {
		t.Fatalf("из доков извлечено %d кандидатов при пороге %d — сломано извлечение, а не доки", total, minDocPathCandidates)
	}
}

var (
	deprecatedConstRe = regexp.MustCompile(`Deprecated\w+\s+DeprecatedPath\s*=\s*"([^"]+)"`)
	canonicalFieldRe  = regexp.MustCompile(`canonical:\s*"([^"]+)"`)
)

func deprecatedFileBody(t *testing.T, tree *Tree) string {
	t.Helper()
	for _, f := range tree.GoFiles {
		if strings.HasSuffix(f.Path, "internal/ingest/deprecated.go") {
			return f.Body
		}
	}
	t.Fatal("не найден internal/ingest/deprecated.go — сторож смотрит мимо дерева")
	return ""
}

func matchedValues(t *testing.T, body string, re *regexp.Regexp, min int, what string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out[normalizeRoutePath(m[1])] = true
	}
	if len(out) < min {
		t.Fatalf("в deprecated.go найдено %d значений (%s) при пороге %d — сломан разбор, а не код", len(out), what, min)
	}
	return out
}

func TestDeprecatedIngestPathsMatchRegistration(t *testing.T) {
	tree := Load(t)
	registered := map[string]bool{}
	for _, r := range registeredIngestRoutes(t) {
		registered[r.path] = true
	}
	body := deprecatedFileBody(t, tree)

	for alias := range matchedValues(t, body, deprecatedConstRe, 3, "константы DeprecatedPath") {
		if !registered[alias] {
			t.Errorf("константа DeprecatedPath = %q не соответствует ни одному зарегистрированному маршруту", alias)
		}
	}
	for canonical := range matchedValues(t, body, canonicalFieldRe, 3, "поля canonical") {
		if !registered[canonical] {
			t.Errorf("canonical %q не соответствует ни одному зарегистрированному маршруту", canonical)
		}
	}
}

const maxUndocumentedIngestRoutes = 3

var undocumentedIngestRoutes = []Exemption{
	{
		Value:   "/api/{project}/envelope/",
		Finding: "маршрут приёма не упомянут в доках",
		Why:     "путь Sentry-совместимости: его формирует SDK из DSN, руками его не зовут, в доках описан DSN, а не адрес",
	},
	{
		Value:   "/api/{project}/store/",
		Finding: "маршрут приёма не упомянут в доках",
		Why:     "путь Sentry-совместимости: его формирует SDK из DSN, руками его не зовут, в доках описан DSN, а не адрес",
	},
	{
		Value:   "/api/v1/{project}/deployments/",
		Finding: "маршрут приёма не упомянут в доках",
		Why:     "форма с завершающим слешем — толерантность для CI-клиентов, не следующих редиректам на POST; тот же адрес, что документированный /api/v1/{project}/deployments, отдельного описания не требует",
	},
}

func TestIngestRoutesAreDocumented(t *testing.T) {
	tree := Load(t)
	body := deprecatedFileBody(t, tree)
	aliases := matchedValues(t, body, deprecatedConstRe, 3, "константы DeprecatedPath")

	byLang := map[string]map[string]bool{}
	for _, lang := range []string{"ru", "en"} {
		byLang[lang] = docPathCandidates(t, tree.Root, lang)
	}

	exempted := ExemptedValues(undocumentedIngestRoutes)
	seen := map[string]bool{}
	for _, r := range registeredIngestRoutes(t) {
		if r.method != http.MethodPost || aliases[r.path] {
			continue
		}
		for _, lang := range []string{"ru", "en"} {
			if byLang[lang][r.path] {
				continue
			}
			seen[r.path] = true
			if exempted[r.path] {
				continue
			}
			t.Errorf("маршрут приёма %q не упомянут в internal/docs/%s", r.path, lang)
		}
	}
	CheckExemptions(t, "undocumented-ingest-routes", undocumentedIngestRoutes, maxUndocumentedIngestRoutes, seen)
}
