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

const maxEscalationSteps = 5

func escalationsPath(projectID int64) string {
	return templates.EscalationsPath(projectID)
}

func escalationsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, escalation.ErrInvalidPolicy):
		return i18n.T(ctx, "err.escalations.invalid")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

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
	// h.EscalationPolicy/h.Alerts nil в узких тестовых стендах — тогда 404, не паника.
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

// Dry-run строится из сохранённой политики, а не черновика формы — предпросмотр того, что уйдёт сейчас.
func (h *Handler) renderEscalations(w http.ResponseWriter, r *http.Request, status int, projectID int64, canManage bool, failedSeverity, errMsg string) {
	ladders, err := h.EscalationPolicy.Ladders(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// channelsForView маскирует Target/зануляет Secret для не-admin до попадания в шаблон.
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

// Ступени с step_no >= maxEscalationSteps (лесенка, заведённая иначе) в форму не попадают,
// сама лесенка не трогается.
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

// Значения берутся буквально из формы, не через ValidateSteps: пустая ступень остаётся
// пустой строкой delay, не "0".
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

// Ступень без канала не входит в результат — это и есть «убрать ступень»; дыру в
// step_no ловит escalation.ValidateSteps.
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

// Отвергаем чужой channel_id ДО SetLadder — тот же контроль есть в сторе
// (defense-in-depth), но здесь отказ приходит с человекочитаемым 422.
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
	if !h.parseForm(w, r) {
		return
	}
	severity := r.FormValue("severity")
	if severity != escalation.SeverityCritical && severity != escalation.SeverityWarning {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}

	steps := escalationStepsFromForm(r)

	// channel_id обязан принадлежать этому проекту — иначе оператор A подставил бы
	// канал B, и уведомления A ушли бы получателю B.
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
