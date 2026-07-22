package web

import (
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// aboutPage — GET /about: сведения о версии/сборке. Доступна любому
// залогиненному пользователю (requireUser, см. web.go); к проекту не привязана.
func (h *Handler) aboutPage(w http.ResponseWriter, r *http.Request) {
	_ = templates.About(version.Get(), h.currentEmail(r)).Render(r.Context(), w)
}
