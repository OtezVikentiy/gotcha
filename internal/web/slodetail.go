package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Инциденты сжигания редки, глубокая история — не задача этой страницы.
const sloDetailIncidentsLimit = 50

// Час, кратный 5м (требование MV transactions_5m) — как в evaluator.fullWindowStep и в
// списке; точности до часа для рендера страницы достаточно.
const sloDetailBurndownStep = time.Hour

func (h *Handler) sloDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.SLO == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	sloID, err := strconv.ParseInt(r.PathValue("sloID"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	s, found, err := h.SLO.Get(r.Context(), projectID, sloID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}

	ctx := r.Context()
	vm := templates.SLODetailVM{
		ProjectID:     projectID,
		ID:            s.ID,
		Name:          s.Name,
		Kind:          string(s.Kind),
		TargetPct:     s.Target * 100,
		WindowDays:    s.WindowDays,
		BurnThreshold: s.BurnThreshold,
	}

	// h.SLOProviders не проведён на стенде без ClickHouse → HasData остаётся false, экран
	// показывает «нет данных», а не 500.
	if p := h.SLOProviders[s.Kind]; p != nil {
		h.fillSLODetailBudget(ctx, &vm, p, s, time.Now().UTC())
	}

	// Ошибка чтения истории не должна ронять экран — тогда просто нет истории.
	if incs, err := h.SLO.Incidents(ctx, projectID, sloID, sloDetailIncidentsLimit); err == nil {
		// Ошибка резолва email не должна ронять экран — строки просто не несут email.
		ackedByIDs := make([]int64, 0, len(incs))
		for _, inc := range incs {
			if inc.AcknowledgedBy != nil {
				ackedByIDs = append(ackedByIDs, *inc.AcknowledgedBy)
			}
		}
		ackedBy, ackErr := h.ackedByEmails(ctx, ackedByIDs)
		if ackErr != nil {
			slog.Warn("slo detail: resolve acknowledged_by emails failed", "project_id", projectID, "slo_id", sloID, "error", ackErr)
		}

		vm.Incidents = make([]templates.SLOIncidentRow, 0, len(incs))
		for _, inc := range incs {
			var ackedByEmail string
			if inc.AcknowledgedBy != nil {
				ackedByEmail = ackedBy[*inc.AcknowledgedBy]
			}
			row := templates.SLOIncidentRow{
				ID:                  inc.ID,
				Open:                inc.Status == "open",
				Severity:            inc.Severity,
				AcknowledgedAt:      inc.AcknowledgedAt,
				AcknowledgedByEmail: ackedByEmail,
				StartedAt:           inc.StartedAt,
				ResolvedAt:          inc.ResolvedAt,
				BurnRate:            inc.BurnRate,
			}
			if inc.BudgetRemaining != nil {
				row.HasBudget = true
				row.BudgetRemainingPct = *inc.BudgetRemaining * 100
			}
			vm.Incidents = append(vm.Incidents, row)
			if row.Open {
				vm.HasOpenIncident = true
				vm.OpenIncident = row
			}
		}
	}

	_ = templates.SLODetailScreen(vm, h.currentEmail(r)).Render(ctx, w)
}

// total==0 за окно / ошибка провайдера → HasData остаётся false: экран покажет «нет данных»,
// не мнимые нули.
func (h *Handler) fillSLODetailBudget(ctx context.Context, vm *templates.SLODetailVM, p slo.Provider, s slo.SLO, now time.Time) {
	from := now.Add(-time.Duration(s.WindowDays) * 24 * time.Hour)
	if capD := p.RetentionCap(); capD > 0 {
		if clip := now.Add(-capD); clip.After(from) {
			from = clip
		}
	}
	bs, err := p.Buckets(ctx, s, from, now, sloDetailBurndownStep)
	if err != nil {
		return
	}
	att, ok := slo.Attainment(bs)
	if !ok {
		return
	}
	rem, _ := slo.BudgetRemainingFraction(bs, s.Target)
	vm.HasData = true
	vm.AttainmentPct = att * 100
	vm.BudgetRemainingPct = rem * 100
	vm.Status = sloStatus(rem)
	vm.Chart = sloBudgetBurndownSVG(ctx, bs, s.Target, sloBurndownWidth, sloBurndownHeight)

	// Длинное окно BurnLongMin с шагом BurnShortMin, короткое — последняя корзина (зеркало
	// evaluator.burnWindows). Отдельный запрос: burn считается по узкому окну, не по полному.
	longMin, shortMin := s.BurnLongMin, s.BurnShortMin
	if longMin <= 0 {
		longMin = 60
	}
	if shortMin <= 0 {
		shortMin = 5
	}
	burnFrom := now.Add(-time.Duration(longMin) * time.Minute)
	bw, err := p.Buckets(ctx, s, burnFrom, now, time.Duration(shortMin)*time.Minute)
	if err != nil {
		return
	}
	if bl, ok := slo.BurnRate(bw, s.Target); ok {
		vm.HasBurn = true
		vm.BurnLong = bl
	}
	if len(bw) > 0 {
		if bshort, ok := slo.BurnRate(bw[len(bw)-1:], s.Target); ok {
			vm.HasBurn = true
			vm.BurnShort = bshort
		}
	}
}
