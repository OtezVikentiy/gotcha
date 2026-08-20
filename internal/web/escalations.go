package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// maxEscalationSteps — число фиксированных строк-ступеней в редакторе, на
// severity. Простой вариант из брифа (без JS-добавления строк): пять ступеней
// (0..4) с запасом хватает любой реалистичной лесенки — эскалация из шести и
// более шагов уже неотличима на глаз оператора от «шлём всем сразу». Ступень
// без выбранного канала считается неиспользуемой и в лесенку не попадает (см.
// escalationStepsFromForm).
const maxEscalationSteps = 5

// escalationsPath — адрес раздела (зеркалит templates.EscalationsPath, тот же
// приём, что и slosPath/templates.SLOsPath): веб-слой не дублирует
// конкатенацию руками, nav и редиректы ссылаются на один канонический адрес.
func escalationsPath(projectID int64) string {
	return templates.EscalationsPath(projectID)
}

// escalationsErrorMessage переводит доменные ошибки PolicyStore.SetLadder в
// человекочитаемое сообщение для 422-страницы, тот же приём, что и
// alertsErrorMessage. Сырой текст ErrInvalidPolicy (английские детали:
// какой именно step_no/delay/канал не прошёл) оператору не показываем —
// вместо него общая подсказка, что именно проверить.
func escalationsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, escalation.ErrInvalidPolicy):
		return i18n.T(ctx, "err.escalations.invalid")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// escalationsPage — GET /projects/{id}/escalations: редактор лесенок
// critical/warning + dry-run-предпросмотр. Доступ — оператор проекта
// (requireProjectOperator, как alerts/slos/metric-alerts): работа с
// эскалациями — операционная задача, не настройка организации.
func (h *Handler) escalationsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.EscalationPolicy/h.Alerts могут быть nil в узких тестовых стендах —
	// тот же nil-guard, что у alertsPage/slosPage, а не паника при
	// разыменовании.
	if h.EscalationPolicy == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderEscalations(w, r, http.StatusOK, projectID, authz.CanManage, "", "")
}

// renderEscalations — общий рендер: GET и POST-обработчик на 422 (тот же
// принцип, что renderAlerts/renderSLOs). failedSeverity — severity формы,
// упавшей на последнем POST: её строки-ступени перерисовываются буквально из
// запроса (введённое не теряется), вторая лесенка — из сохранённой политики.
// Пустая failedSeverity — обычный GET, обе лесенки из PolicyStore.
//
// Dry-run-блок ВСЕГДА строится из фактической (сохранённой) политики — из тех
// же ladders, что и формы при успехе — а не из непринятого черновика формы:
// это предпросмотр того, что разошлётся ПРЯМО СЕЙЧАС, а не того, что
// оператор пытался, но не смог сохранить.
func (h *Handler) renderEscalations(w http.ResponseWriter, r *http.Request, status int, projectID int64, canManage bool, failedSeverity, errMsg string) {
	ladders, err := h.EscalationPolicy.Ladders(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Каналы — через единственную дверь чтения (channelsForView, находка B1):
	// маскирует Target/зануляет Secret для не-admin, прежде чем список дойдёт
	// до шаблона.
	channels, err := h.channelsForView(r.Context(), projectID, canManage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	criticalForm := ladderToForm(escalation.SeverityCritical, ladders[escalation.SeverityCritical])
	warningForm := ladderToForm(escalation.SeverityWarning, ladders[escalation.SeverityWarning])
	switch failedSeverity {
	case escalation.SeverityCritical:
		criticalForm = escalationFormFromRequest(r, escalation.SeverityCritical)
	case escalation.SeverityWarning:
		warningForm = escalationFormFromRequest(r, escalation.SeverityWarning)
	}

	w.WriteHeader(status)
	_ = templates.Escalations(projectID, channels, criticalForm, warningForm, ladders, failedSeverity, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// ladderToForm — вью-модель фиксированных maxEscalationSteps строк формы из
// сохранённой лесенки: незанятые ступени остаются пустыми строками (нет
// такого step_no в ladder). Ступени за пределами maxEscalationSteps (в
// принципе недостижимо через этот редактор, но теоретически достижимо, если
// лесенку когда-то завели иначе) в форму не попадают — сама лесенка при этом
// не трогается, пока оператор её не пересохранит.
func ladderToForm(severity string, ladder escalation.Ladder) templates.EscalationLadderForm {
	steps := make([]templates.EscalationStepForm, maxEscalationSteps)
	for i := range steps {
		steps[i] = templates.EscalationStepForm{StepNo: i, Selected: map[int64]bool{}}
	}
	for _, st := range ladder {
		if st.StepNo >= 0 && st.StepNo < maxEscalationSteps {
			steps[st.StepNo] = templates.EscalationStepForm{
				StepNo:       st.StepNo,
				DelayMinutes: strconv.Itoa(st.DelayMinutes),
				Selected:     toInt64Set(st.ChannelIDs),
			}
		}
	}
	return templates.EscalationLadderForm{Severity: severity, Steps: steps}
}

// escalationFormFromRequest — та же вью-модель, но из отправленной (и не
// сохранившейся) формы: значения берутся буквально, не через ValidateSteps —
// пустая ступень так и остаётся пустой строкой delay, а не "0".
func escalationFormFromRequest(r *http.Request, severity string) templates.EscalationLadderForm {
	steps := make([]templates.EscalationStepForm, maxEscalationSteps)
	for i := range steps {
		ids := parseInt64List(r.PostForm[stepChannelsField(i)])
		steps[i] = templates.EscalationStepForm{
			StepNo:       i,
			DelayMinutes: r.FormValue(stepDelayField(i)),
			Selected:     toInt64Set(ids),
		}
	}
	return templates.EscalationLadderForm{Severity: severity, Steps: steps}
}

func stepDelayField(i int) string    { return fmt.Sprintf("step%d_delay", i) }
func stepChannelsField(i int) string { return fmt.Sprintf("step%d_channels", i) }

// escalationStepsFromForm читает maxEscalationSteps фиксированных строк формы
// и строит []escalation.Step для SetLadder. Строка без единого выбранного
// канала считается неиспользуемой ступенью и в результат не попадает — это и
// есть «убрать ступень» простым способом (без JS): оставшиеся ступени со
// своими исходными step_no могут после этого образовать дыру, которую
// отловит escalation.ValidateSteps (422, не запись мимо проверки).
func escalationStepsFromForm(r *http.Request) []escalation.Step {
	var steps []escalation.Step
	for i := 0; i < maxEscalationSteps; i++ {
		ids := parseInt64List(r.PostForm[stepChannelsField(i)])
		if len(ids) == 0 {
			continue
		}
		steps = append(steps, escalation.Step{
			StepNo:       i,
			DelayMinutes: formInt(r, stepDelayField(i)),
			ChannelIDs:   ids,
		})
	}
	return steps
}

// foreignChannelStep — первый channel_id формы, не принадлежащий проекту
// (concern T2, cross-tenant): valid — множество ID каналов ЭТОГО проекта
// (channelsForView выше). Хендлер отвергает такую отправку ДО похода в
// PolicyStore.SetLadder — тот же results на defense-in-depth в самом сторе
// (policy.go, verifyChannelsBelongToProject), но здесь отказ приходит с
// человекочитаемым 422, а не голой ошибкой БД.
func foreignChannelStep(steps []escalation.Step, valid map[int64]bool) (int64, bool) {
	for _, st := range steps {
		for _, id := range st.ChannelIDs {
			if !valid[id] {
				return id, true
			}
		}
	}
	return 0, false
}

// escalationsSave — POST /projects/{id}/escalations: сохраняет ОДНУ лесенку
// за раз (поле формы severity=critical|warning — два отдельных сабмита на
// странице), тем же принципом, что и alertsRulesSave/sloCreate: сначала
// cross-tenant фильтр по каналам проекта, затем SetLadder (со своим
// defense-in-depth того же контроля).
func (h *Handler) escalationsSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.EscalationPolicy == nil || h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	severity := r.FormValue("severity")
	if severity != escalation.SeverityCritical && severity != escalation.SeverityWarning {
		http.Error(w, "bad severity", http.StatusBadRequest)
		return
	}

	steps := escalationStepsFromForm(r)

	// Cross-tenant (concern T2, ОБЯЗАТЕЛЬНО): channel_id из формы обязан
	// принадлежать ЭТОМУ проекту — иначе оператор проекта A подобранным id
	// прицепил бы к своей лесенке канал проекта B, и уведомления инцидентов A
	// уходили бы получателю, которым управляет и которого видит B.
	channels, err := h.channelsForView(r.Context(), projectID, authz.CanManage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	valid := make(map[int64]bool, len(channels))
	for _, c := range channels {
		valid[c.ID] = true
	}
	if _, foreign := foreignChannelStep(steps, valid); foreign {
		h.renderEscalations(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, severity,
			i18n.T(r.Context(), "err.escalations.foreign_channel"))
		return
	}

	if err := h.EscalationPolicy.SetLadder(r.Context(), projectID, severity, steps); err != nil {
		h.renderEscalations(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, severity,
			escalationsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.escalations_saved", 0)
	http.Redirect(w, r, escalationsPath(projectID), http.StatusSeeOther)
}
