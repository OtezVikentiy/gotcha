package docs

import (
	"bytes"
	"context"
	"embed"
	"html"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

//go:embed ru/*.md en/*.md
var files embed.FS

type Page struct {
	Slug  string
	Group string // i18n-ключ группы для индекса
	Title string
}

var registry = []struct{ Slug, Group string }{
	{"getting-started", "docs.group.start"},
	{"keys", "docs.group.start"},
	{"glossary", "docs.group.start"},
	{"time-range", "docs.group.start"},
	{"installation", "docs.group.deploy"},
	{"installation-bare-metal", "docs.group.deploy"},
	{"configuration", "docs.group.deploy"},
	{"hardening", "docs.group.deploy"},
	{"backup-restore", "docs.group.deploy"},
	{"upgrade", "docs.group.deploy"},
	{"versioning", "docs.group.deploy"},
	{"self-monitoring", "docs.group.deploy"},
	{"cardinality", "docs.group.deploy"},
	{"overview", "docs.group.sections"},
	{"issues", "docs.group.sections"},
	{"exports", "docs.group.sections"},
	{"performance", "docs.group.sections"},
	{"dependencies", "docs.group.sections"},
	{"deployments", "docs.group.sections"},
	{"slo", "docs.group.sections"},
	{"metrics", "docs.group.sections"},
	{"recipes", "docs.group.sections"},
	{"metric-alerts", "docs.group.sections"},
	{"hosts", "docs.group.sections"},
	{"logs", "docs.group.sections"},
	{"profiling", "docs.group.sections"},
	{"uptime", "docs.group.sections"},
	{"status-pages", "docs.group.sections"},
	{"maintenance", "docs.group.sections"},
	{"probes", "docs.group.sections"},
	{"alerts", "docs.group.sections"},
	{"escalations", "docs.group.sections"},
	{"alert-suppression", "docs.group.sections"},
	{"incident-groups", "docs.group.sections"},
	{"teams", "docs.group.admin"},
	{"sso", "docs.group.admin"},
	{"privacy", "docs.group.admin"},
	{"sdk", "docs.group.integrations"},
}

// Та же обёртка-скролл, что scrollRegion в шаблонах: role=table возвращается в
// дерево доступности (display:block с таблицы снят в app.css).
type docsTableRenderer struct{ label string }

func (r *docsTableRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(extast.KindTable, r.renderTable)
}

func (r *docsTableRenderer) renderTable(w util.BufWriter, _ []byte, _ gast.Node, entering bool) (gast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString(`<div class="table-scroll" tabindex="0" role="region" aria-label="` + html.EscapeString(r.label) + `"><table>`)
	} else {
		_, _ = w.WriteString(`</table></div>`)
	}
	return gast.WalkContinue, nil
}

// WithUnsafe НЕ включён: raw HTML в markdown экранируется, а не рендерится как есть.
var (
	mdMu    sync.Mutex
	mdByLoc = map[string]goldmark.Markdown{}
)

func mdFor(loc string) goldmark.Markdown {
	mdMu.Lock()
	defer mdMu.Unlock()
	if m, ok := mdByLoc[loc]; ok {
		return m
	}
	label := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: loc}), "docs.table_region")
	m := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(
			util.Prioritized(&docsTableRenderer{label: label}, 100),
		)),
	)
	mdByLoc[loc] = m
	return m
}

type rendered struct {
	html  string
	title string
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]rendered{} // ключ "loc/slug"
)

func normalizeLocale(loc string) string {
	if loc == "en" {
		return "en"
	}
	return "ru"
}

func known(slug string) bool {
	for _, r := range registry {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

func Render(locale, slug string) (string, string, bool) {
	if !known(slug) {
		return "", "", false
	}
	loc := normalizeLocale(locale)
	key := loc + "/" + slug
	cacheMu.RLock()
	if r, ok := cache[key]; ok {
		cacheMu.RUnlock()
		return r.html, r.title, true
	}
	cacheMu.RUnlock()

	data, err := files.ReadFile(loc + "/" + slug + ".md")
	if err != nil && loc != "ru" {
		data, err = files.ReadFile("ru/" + slug + ".md")
	}
	if err != nil {
		return "", "", false
	}
	title := firstH1(data)
	var buf bytes.Buffer
	// Генератор якорей свой на каждую страницу — общий приписывал бы суффиксы
	// «-1», «-2» заголовкам разных документов.
	ctx := parser.NewContext(parser.WithIDs(newTranslitIDs()))
	if err := mdFor(loc).Convert(data, &buf, parser.WithContext(ctx)); err != nil {
		return "", "", false
	}
	r := rendered{html: buf.String(), title: title}
	cacheMu.Lock()
	cache[key] = r
	cacheMu.Unlock()
	return r.html, r.title, true
}

func firstH1(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(t[2:])
		}
	}
	return ""
}

func Pages(locale string) []Page {
	loc := normalizeLocale(locale)
	out := make([]Page, 0, len(registry))
	for _, r := range registry {
		data, err := files.ReadFile(loc + "/" + r.Slug + ".md")
		if err != nil && loc != "ru" {
			data, _ = files.ReadFile("ru/" + r.Slug + ".md")
		}
		out = append(out, Page{Slug: r.Slug, Group: r.Group, Title: firstH1(data)})
	}
	return out
}
