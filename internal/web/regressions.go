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

// regressionsPreFilterLimit — потолок выборки из List ДО фильтрации по статусу
// в Go. RegressionService.List не принимает статус, а страница по умолчанию
// показывает только открытые, поэтому берём с запасом (открытых на цель — не
// более одной, закрытых со временем накапливается больше), чтобы фильтр open не
// оставался пустым из-за преобладания resolved в начале ORDER BY started_at DESC.
const regressionsPreFilterLimit = 500

// regressionsListLimit — сколько строк показываем после фильтрации по статусу.
const regressionsListLimit = 100

// regressionDeployWindow — насколько далеко назад от начала регрессии ищем
// предшествующий деплой для привязки «что изменилось перед сбоем». Деплой
// старше окна с регрессией уже не связан — за неделю накатывается что угодно.
const regressionDeployWindow = 7 * 24 * time.Hour

func regressionsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/regressions"
}

// regressionStatusFilter переводит query-параметр status в имя фильтра для формы
// и предикат для отбора в Go. Дефолт (пустой или неизвестный) — open: страница
// по умолчанию показывает то, что сейчас регрессирует. "all" — без фильтра.
func regressionStatusFilter(v string) (name string, keep func(status string) bool) {
	switch v {
	case "resolved":
		return "resolved", func(s string) bool { return s == "resolved" }
	case "all":
		return "all", func(string) bool { return true }
	default:
		return "open", func(s string) bool { return s == "open" }
	}
}

// regressionsList — GET /projects/{id}/regressions: таблица регрессий
// производительности проекта (цель, метрика, рост %, база→пик, статус,
// длительность). Доступ — CanAccessProject, иначе 404 (тот же принцип, что и у
// perfIssuesList); только чтение, POST'ов и sameOrigin здесь нет — регрессии
// закрываются оценщиком автоматически.
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
	// h.Regressions может быть nil в стендах без детекции — тогда 404, как и при
	// отсутствии доступа (тот же приём, что и nil-guard на h.PerfIssues), а не
	// паника при разыменовании.
	if h.Regressions == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	// CanOperate — read-only, тот же приём, что hostDetail (renderHostDetail):
	// список открыт всем участникам проекта (CanAccessProject выше), ack-кнопка
	// на строке открытой регрессии — только оператору.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	filterName, keep := regressionStatusFilter(r.URL.Query().Get("status"))
	all, err := h.Regressions.List(r.Context(), projectID, regressionsPreFilterLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Фильтр по статусу — в Go: List статус не принимает (см. брифинг), поэтому
	// отбираем нужные и режем до потолка отображения.
	items := make([]trace.Regression, 0, len(all))
	for _, reg := range all {
		if keep(reg.Status) {
			items = append(items, reg)
			if len(items) >= regressionsListLimit {
				break
			}
		}
	}

	deployAttr := regressionDeployAttribution(r.Context(), h.Deploy, projectID, items)

	// Индикатор сезонного режима детекции берём из конфига проекта. Ошибка чтения
	// проекта не должна ронять список — бейдж декоративен: тогда seasonal=false.
	seasonal := false
	if project, err := h.Org.GetProject(r.Context(), projectID); err == nil {
		if cfg, cErr := trace.RegressionConfigFromJSON([]byte(project.PerfRegressionConfig)); cErr == nil {
			seasonal = cfg.SeasonalEnabled
		}
	}

	// ackedBy — W2-C находка 4: email подтвердившего, батчем (см. ackedByEmails).
	ackedByIDs := make([]int64, 0, len(items))
	for _, reg := range items {
		if reg.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *reg.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	_ = templates.RegressionsList(projectID, items, deployAttr, filterName, h.currentEmail(r), seasonal, canOperate, ackedBy).Render(r.Context(), w)
}

// regressionDeployAttribution для каждой регрессии из items возвращает текст
// «после деплоя vX (когда)» ближайшего ПРЕДШЕСТВУЮЩЕГО деплоя (deployed_at <=
// started_at, в пределах regressionDeployWindow), либо "" если такого нет. Срез
// той же длины и порядка, что items — параллелен строкам таблицы.
//
// Один запрос к стору деплоев на весь список: окно List покрывает все
// показываемые регрессии (от самой ранней минус окно привязки до now), а сама
// привязка ближайшего предшествующего деплоя считается в Go — без N+1 по
// строкам. Ошибка стора не роняет страницу: привязка декоративна, без неё
// таблица регрессий остаётся полной.
func regressionDeployAttribution(ctx context.Context, store *deploy.Store, projectID int64, items []trace.Regression) []string {
	attr := make([]string, len(items))
	if store == nil || len(items) == 0 {
		return attr
	}

	// Нижняя граница окна выборки — начало самой ранней регрессии минус окно
	// привязки: раньше него ни одна регрессия не может привязаться к деплою.
	minStarted := items[0].StartedAt
	for _, reg := range items[1:] {
		if reg.StartedAt.Before(minStarted) {
			minStarted = reg.StartedAt
		}
	}
	from := minStarted.Add(-regressionDeployWindow)
	// Верхняя граница List — эксклюзивна; берём now с запасом, чтобы деплой,
	// совпавший по времени с концом окна, в выборку попал.
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

// nearestPrecedingDeploy ищет в deploys ближайший деплой ПЕРЕД started (или
// ровно в его момент) в пределах regressionDeployWindow — тот, после которого
// началась регрессия. deploys приходит из List newest-first, но опираться на
// порядок не станем: явно максимизируем DeployedAt.
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
