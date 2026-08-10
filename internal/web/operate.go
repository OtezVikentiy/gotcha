package web

import (
	"context"
	"errors"
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// canOperateProject — «оператор мониторинга» проекта: владелец/админ
// организации ИЛИ участник команды, прикреплённой к проекту. Условие
// сегодня совпадает с CanAccessProject, но имя отдельное осознанно:
// «видеть» и «оперировать» — разные понятия, и склеенные одним именем
// они сломаются при первом же ужесточении одного из них (спека
// cld/plans/2026-08-08-access-model-rework.md).
func (h *Handler) canOperateProject(ctx context.Context, projectID, userID int64) (bool, error) {
	return h.Org.CanAccessProject(ctx, userID, projectID)
}

// requireProjectOperator — гейт «оператора»: резолвит projectID → orgID
// (несуществующий проект → 404) и требует canOperateProject. Отказ —
// стилизованная 404, как у requireProjectRole для не-члена: у кого нет
// доступа к проекту, для того не существует и сам проект (единый
// existence-oracle). Случая «доступ есть, оператор — нет» сегодня не
// существует (предикаты совпадают); если они разойдутся — для члена с
// доступом здесь должен появиться честный 403 (№72), не 404.
func (h *Handler) requireProjectOperator(w http.ResponseWriter, r *http.Request, projectID, userID int64) (int64, bool) {
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, false
	}
	ok, err := h.canOperateProject(r.Context(), projectID, userID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, false
	}
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return 0, false
	}
	return orgID, true
}
