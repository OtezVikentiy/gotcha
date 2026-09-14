package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const regressionsListLimit = 100

// Деплой старше окна с регрессией уже не связан — за неделю накатывается что угодно.
const regressionDeployWindow = 7 * 24 * time.Hour

func regressionsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/regressions"
}

func regressionStatusFilter(v string) string {
	switch v {
	case "resolved":
		return "resolved"
	case "all":
		return "all"
	default:
		return "open"
	}
}

func (h *Handler) regressionsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Regressions == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	// Список открыт всем участникам проекта, ack-кнопка на открытой регрессии — только оператору.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	filterName := regressionStatusFilter(r.URL.Query().Get("status"))
	items, err := h.Regressions.List(r.Context(), projectID, filterName, regressionsListLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	deployAttr := regressionDeployAttribution(r.Context(), h.Deploy, projectID, items)

	// Ошибка чтения проекта не роняет список — бейдж декоративен, тогда seasonal=false.
	seasonal := false
	if project, err := h.Org.GetProject(r.Context(), projectID); err == nil {
		if cfg, cErr := trace.RegressionConfigFromJSON([]byte(project.PerfRegressionConfig)); cErr == nil {
			seasonal = cfg.SeasonalEnabled
		}
	}

	ackedByIDs := make([]int64, 0, len(items))
	for _, reg := range items {
		if reg.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *reg.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	_ = templates.RegressionsList(projectID, items, deployAttr, filterName, h.currentEmail(r), seasonal, canOperate, ackedBy).Render(r.Context(), w)
}

// Один запрос к стору деплоев на весь список — без N+1 по строкам; ошибка стора не роняет
// страницу, привязка декоративна.
func regressionDeployAttribution(ctx context.Context, store *deploy.Store, projectID int64, items []trace.Regression) []string {
	attr := make([]string, len(items))
	if store == nil || len(items) == 0 {
		return attr
	}

	// Раньше этой границы ни одна регрессия не может привязаться к деплою.
	minStarted := items[0].StartedAt
	for _, reg := range items[1:] {
		if reg.StartedAt.Before(minStarted) {
			minStarted = reg.StartedAt
		}
	}
	from := minStarted.Add(-regressionDeployWindow)
	// Верхняя граница List эксклюзивна — добавляем запас, чтобы деплой на границе окна не выпал.
	to := time.Now().Add(time.Minute)

	deploys, err := store.List(ctx, projectID, from, to, 0)
	if err != nil {
		return attr
	}

	for i, reg := range items {
		best, found := nearestPrecedingDeploy(deploys, reg.StartedAt)
		if !found {
			continue
		}
		attr[i] = i18n.Tf(ctx, "regressions.after_deploy", "version", best.Version) +
			" (" + humanize.Ago(ctx, best.DeployedAt) + ")"
	}
	return attr
}

// deploys приходит newest-first из List, но на порядок не полагаемся — явно максимизируем
// DeployedAt среди кандидатов в пределах окна.
func nearestPrecedingDeploy(deploys []deploy.Deployment, started time.Time) (deploy.Deployment, bool) {
	var best deploy.Deployment
	found := false
	for _, d := range deploys {
		if d.DeployedAt.After(started) {
			continue
		}
		if started.Sub(d.DeployedAt) > regressionDeployWindow {
			continue
		}
		if !found || d.DeployedAt.After(best.DeployedAt) {
			best = d
			found = true
		}
	}
	return best, found
}
