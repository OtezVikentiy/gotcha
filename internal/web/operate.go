package web

import (
	"context"
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

func (h *Handler) canOperateProject(ctx context.Context, projectID, userID int64) (bool, error) {
	return h.Org.CanAccessProject(ctx, userID, projectID)
}

type projectAuthz struct {
	OrgID     int64
	CanManage bool
}

// Пока canOperateProject совпадает с CanAccessProject; если разойдутся, для
// «доступ есть, оператор нет» нужен честный 403, а не текущий 404.
func (h *Handler) requireProjectOperator(w http.ResponseWriter, r *http.Request, projectID, userID int64) (projectAuthz, bool) {
	orgID, ok := h.projectOrgOr404(w, r, projectID)
	if !ok {
		return projectAuthz{}, false
	}
	canOperate, err := h.canOperateProject(r.Context(), projectID, userID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return projectAuthz{}, false
	}
	if !canOperate {
		h.renderError(w, r, http.StatusNotFound, "")
		return projectAuthz{}, false
	}
	// CanManage считается безусловно, даже для вызовов, которым он не нужен —
	// чтобы не раздваивать гейт ради редких путей.
	canManage, err := h.canManageOrg(r.Context(), orgID, userID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return projectAuthz{}, false
	}
	return projectAuthz{OrgID: orgID, CanManage: canManage}, true
}

// Единственная точка чтения каналов проекта: маскирует Target/Secret не-admin.
// Прямой вызов h.Alerts.Channels вне guard-списка не пройдёт сборку тестов.
func (h *Handler) channelsForView(ctx context.Context, projectID int64, canManage bool) ([]alert.Channel, error) {
	channels, err := h.Alerts.Channels(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !canManage {
		for i := range channels {
			channels[i].Target = maskChannelTarget(channels[i].Kind, channels[i].Target)
			channels[i].Secret = ""
		}
	}
	return channels, nil
}
