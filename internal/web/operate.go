package web

import (
	"context"
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
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

// projectAuthz — то, что requireProjectOperator узнаёт о вызывающем за один
// проход: orgID проекта и уже посчитанный CanManage (owner/admin этой
// организации). Раньше рендеры, которым нужен был ещё и CanManage, звали
// canManageProject отдельно — тот заново резолвил projectID -> orgID и
// перечитывал роль, хотя гейт секундой раньше уже сделал то же самое
// (находка B4, спека cld/plans/2026-08-08-access-model-rework.md).
type projectAuthz struct {
	OrgID     int64
	CanManage bool
}

// requireProjectOperator — гейт «оператора»: резолвит projectID → orgID
// (несуществующий проект → 404, projectOrgOr404) и требует canOperateProject.
// Отказ — стилизованная 404, как у requireProjectRole для не-члена: у кого
// нет доступа к проекту, для того не существует и сам проект (единый
// existence-oracle). Случая «доступ есть, оператор — нет» сегодня не
// существует (предикаты совпадают); если они разойдутся — для члена с
// доступом здесь должен появиться честный 403 (№72), не 404. CanManage
// считается тут же, на уже известном orgID (canManageOrg), чтобы рендеры не
// делали это заново отдельным canManageProject.
func (h *Handler) requireProjectOperator(w http.ResponseWriter, r *http.Request, projectID, userID int64) (projectAuthz, bool) {
	orgID, ok := h.projectOrgOr404(w, r, projectID)
	if !ok {
		return projectAuthz{}, false
	}
	canOperate, err := h.canOperateProject(r.Context(), projectID, userID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return projectAuthz{}, false
	}
	if !canOperate {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return projectAuthz{}, false
	}
	// Считаем CanManage БЕЗУСЛОВНО, для всех вызывающих — в том числе тех, кому
	// он не нужен (pause/resume/delete монитора, maintenance.go, metricalerts.go
	// берут отсюда только ok/OrgID). Осознанно: два варианта гейта (с CanManage
	// и без) убрали бы этот один Org.Role() на путях, где он не нужен, но
	// мутации монитора/окон/правил редки — не стоят раздвоения гейта. Взамен —
	// новая (маловероятная) 500-точка отказа на этих путях: раньше Org.Role()
	// здесь не вызывался вовсе, теперь вызывается всегда. Плата за то, что
	// рендеры больше не резолвят CanManage повторно (находка B4).
	canManage, err := h.canManageOrg(r.Context(), orgID, userID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return projectAuthz{}, false
	}
	return projectAuthz{OrgID: orgID, CanManage: canManage}, true
}

// channelsForView — единственная дверь к каналам проекта для ЧТЕНИЯ (находка
// B1, спека 2026-08-08): раньше маскировка Target/Secret для не-admin была
// продублирована на каждом сайте рендера отдельно (renderAlerts,
// renderMonitorForm) — новый сайт, который забыл бы про маску, тихо
// показал бы оператору сырой адрес/секрет. Теперь это одна функция, и
// сорс-guard (internal/guards) держит список мест, которым разрешено звать
// h.Alerts.Channels напрямую, в четырёх записях (admin channel-CRUD) — любой
// пятый вызов красный уже на этапе сборки тестов.
//
// canManage приходит от вызывающего — гейт (requireProjectOperator, находка
// B4) его уже посчитал, здесь заново не резолвится.
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
