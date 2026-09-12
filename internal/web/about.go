package web

import (
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// доступна любому залогиненному пользователю, к проекту не привязана.
func (h *Handler) aboutPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.About(version.Get(), h.currentEmail(r)).Render(r.Context(), w)
}
