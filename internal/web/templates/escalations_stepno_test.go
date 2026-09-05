package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestEscalationStepDisplayedOneBased — K9-16: в данных ступени 0-based
// (StepNo, имена полей формы), человеку показывается 1-based: первая
// ступень — «Ступень 1», и в легенде редактора, и в строке dry-run. Имена
// полей формы при этом остаются по StepNo — обработчик их парсит как есть.
func TestEscalationStepDisplayedOneBased(t *testing.T) {
	ch := []alert.Channel{{ID: 1, Kind: "email", Enabled: true, Target: "a@b.c"}}
	form := renderTo(t, escalationStepFields(EscalationStepForm{StepNo: 0, DelayMinutes: "0", Selected: map[int64]bool{1: true}}, ch))
	if !strings.Contains(form, "Ступень 1</legend>") {
		t.Errorf("первая ступень в легенде должна быть «Ступень 1»: %s", form)
	}
	if strings.Contains(form, "Ступень 0") {
		t.Errorf("сырой 0-based индекс утёк в легенду: %s", form)
	}
	if !strings.Contains(form, `name="`+stepDelayField(0)+`"`) {
		t.Errorf("имя поля формы обязано остаться по StepNo=0: %s", form)
	}

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	dry := escalationDryRunStepText(ctx, escalation.Step{StepNo: 0, ChannelIDs: []int64{1}}, ch)
	if !strings.HasPrefix(dry, "Ступень 1 ") {
		t.Errorf("dry-run первой ступени: %q, want префикс «Ступень 1 »", dry)
	}
	delayed := escalationDryRunStepText(ctx, escalation.Step{StepNo: 2, DelayMinutes: 5, ChannelIDs: []int64{1}}, ch)
	if !strings.HasPrefix(delayed, "Ступень 3 ") {
		t.Errorf("dry-run третьей ступени: %q, want префикс «Ступень 3 »", delayed)
	}
}

// TestEscalationsPageExplainsIssueAlerts — K1-3: страница обещает лесенку
// «инциденту», а алерты по проблемам (alert/evaluator.go → escalation.Dispatch
// напрямую) её не проходят — справка обязана это оговаривать.
func TestEscalationsPageExplainsIssueAlerts(t *testing.T) {
	out := renderTo(t, Escalations(7, nil, EscalationLadderForm{Severity: "critical"}, EscalationLadderForm{Severity: "warning"}, map[string]escalation.Ladder{}, "", "", "u@e.com"))
	if !strings.Contains(out, "Алерты по проблемам") || !strings.Contains(out, "лесенку не проходят") {
		t.Errorf("справка эскалаций без оговорки про алерты по проблемам: %s", out)
	}
}
