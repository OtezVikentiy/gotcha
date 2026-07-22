// Package docs — встроенная (go:embed) markdown-документация Gotcha,
// рендер через goldmark в безопасный HTML. Контент — internal/docs/{ru,en}/*.md.
package docs

import (
	"bytes"
	"embed"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
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
	{"glossary", "docs.group.start"},
	// Установка и эксплуатация
	{"installation", "docs.group.deploy"},
	{"configuration", "docs.group.deploy"},
	{"backup-restore", "docs.group.deploy"},
	{"upgrade", "docs.group.deploy"},
	// Разделы
	{"issues", "docs.group.sections"},
	{"performance", "docs.group.sections"},
	{"metrics", "docs.group.sections"},
	{"metric-alerts", "docs.group.sections"},
	{"profiling", "docs.group.sections"},
	{"uptime", "docs.group.sections"},
	{"status-pages", "docs.group.sections"},
	{"maintenance", "docs.group.sections"},
	{"probes", "docs.group.sections"},
	{"alerts", "docs.group.sections"},
	// Администрирование
	{"teams", "docs.group.admin"},
	{"sso", "docs.group.admin"},
	{"privacy", "docs.group.admin"},
	// Интеграции
	{"sdk", "docs.group.integrations"},
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM)) // GFM: таблицы/автоссылки; без WithUnsafe → raw HTML экранируется

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
	if err := md.Convert(data, &buf); err != nil {
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
