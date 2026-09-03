// Package docs — встроенная (go:embed) markdown-документация Gotcha,
// рендер через goldmark в безопасный HTML. Контент — internal/docs/{ru,en}/*.md.
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

// Page — запись реестра: slug, i18n-ключ группы и заголовок (H1) в текущей локали.
type Page struct {
	Slug  string
	Group string // i18n-ключ группы для индекса
	Title string
}

// registry — порядок и группировка страниц оглавления (Title заполняется из H1).
var registry = []struct{ Slug, Group string }{
	// Начало
	{"getting-started", "docs.group.start"},
	// keys — концептуальная страница уровня glossary/time-range (что такое
	// тип ключа и какой чему разрешён), а не факт про конкретную интеграцию:
	// ключ нужен раньше любого из разделов ниже, включая SDK, поэтому здесь,
	// а не в docs.group.integrations рядом с sdk.
	{"keys", "docs.group.start"},
	{"glossary", "docs.group.start"},
	{"time-range", "docs.group.start"},
	// Установка и эксплуатация
	{"installation", "docs.group.deploy"},
	{"configuration", "docs.group.deploy"},
	{"hardening", "docs.group.deploy"},
	{"backup-restore", "docs.group.deploy"},
	{"upgrade", "docs.group.deploy"},
	{"versioning", "docs.group.deploy"},
	{"self-monitoring", "docs.group.deploy"},
	{"cardinality", "docs.group.deploy"},
	// Разделы
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
	// Администрирование
	{"teams", "docs.group.admin"},
	{"sso", "docs.group.admin"},
	{"privacy", "docs.group.admin"},
	// Интеграции
	{"sdk", "docs.group.integrations"},
}

// docsTableRenderer оборачивает каждую таблицу в тот же скролл-контейнер, что
// scrollRegion в шаблонах (№31/№75): role=table возвращается в дерево
// доступности (display:block с таблицы снят в app.css), а прокрутка и
// клавиатурный доступ живут на обёртке.
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

// mdFor — рендерер документации для локали: подпись региона таблицы берётся
// из каталога, сам goldmark собирается один раз на локаль.
//
// GFM даёт таблицы и автоссылки; WithUnsafe НЕ включён, поэтому raw HTML
// экранируется. WithAutoHeadingID проставляет заголовкам id: без него якорных
// ссылок не существовало вовсе, при том что тексты уже ссылаются на разделы
// прозой («см. раздел о внешних получателях ниже»), а браузерная кнопка
// «поделиться ссылкой на этот абзац» не работала ни на одной странице.
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
	return "ru" // дефолт и fallback
}

func known(slug string) bool {
	for _, r := range registry {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

// Render рендерит markdown-страницу локали в безопасный HTML.
// Возвращает (html, title, ok). Неизвестный slug → ok=false.
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

	// читаем запрошенную локаль, затем ru-fallback
	data, err := files.ReadFile(loc + "/" + slug + ".md")
	if err != nil && loc != "ru" {
		data, err = files.ReadFile("ru/" + slug + ".md")
	}
	if err != nil {
		return "", "", false
	}
	title := firstH1(data)
	var buf bytes.Buffer
	// Генератор якорей — на КАЖДУЮ страницу свой: он ведёт список уже занятых
	// идентификаторов, и общий на все страницы начал бы приписывать суффиксы
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

// firstH1 возвращает текст первого "# " заголовка markdown (для title/TOC).
func firstH1(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(t[2:])
		}
	}
	return ""
}

// Pages возвращает упорядоченный реестр страниц с заголовками (H1) в локали.
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
