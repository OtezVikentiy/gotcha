package web

import (
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// docsIndex — GET /docs: список документации, сгруппированный по секциям
// (Начало/Разделы/Интеграции). Доступен любому залогиненному юзеру — docs не
// привязаны к проекту/организации, requireUser (см. web.go) — единственная
// проверка доступа.
func (h *Handler) docsIndex(w http.ResponseWriter, r *http.Request) {
	loc := i18n.FromContext(r.Context()).Code
	_ = templates.DocsIndex(groupDocsPages(docs.Pages(loc)), h.currentEmail(r)).Render(r.Context(), w)
}

// docsPage — GET /docs/{slug}: отрендеренная markdown-страница документации
// (docs.Render, задача 1). Неизвестный slug — единообразный 404 (тот же
// приём, что и у остального кабинета: не палить существование отдельной
// страницей ошибки, см. c014535 — uniform 404 on perf-issue status route).
func (h *Handler) docsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	loc := i18n.FromContext(r.Context()).Code
	html, title, ok := docs.Render(loc, slug)
	if !ok {
		h.notFound(w, r)
		return
	}
	_ = templates.DocsPage(slug, title, html, docs.Pages(loc), h.currentEmail(r)).Render(r.Context(), w)
}

// groupDocsPages режет упорядоченный docs.Pages() на группы по Group
// (i18n-ключ заголовка секции индекса), не переупорядочивая ни группы, ни
// страницы внутри них — реестр в internal/docs уже упорядочен по секциям
// (Начало → Разделы → Интеграции).
func groupDocsPages(pages []docs.Page) []templates.DocsGroup {
	var groups []templates.DocsGroup
	for _, p := range pages {
		if len(groups) == 0 || groups[len(groups)-1].Key != p.Group {
			groups = append(groups, templates.DocsGroup{Key: p.Group})
		}
		groups[len(groups)-1].Pages = append(groups[len(groups)-1].Pages, p)
	}
	return groups
}
