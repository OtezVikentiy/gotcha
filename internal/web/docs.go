package web

import (
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func (h *Handler) docsIndex(w http.ResponseWriter, r *http.Request) {
	loc := i18n.FromContext(r.Context()).Code
	_ = templates.DocsIndex(groupDocsPages(docs.Pages(loc)), h.currentEmail(r)).Render(r.Context(), w)
}

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
