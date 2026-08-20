package recipes

import (
	"context"
	"fmt"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

// RuleStatus — статус одного RuleSpec рецепта против существующих правил
// проекта: Exists=true, если правило с тем же полным ключом уже есть.
type RuleStatus struct {
	Spec   RuleSpec
	Exists bool
}

// matches — ключ идемпотентности рецепта: (MetricName, Aggregation,
// Comparator, LabelKey, LabelValue) при existing.Environment == "".
// Threshold и WindowSeconds в ключ НЕ входят: пользователь мог подстроить
// порог под себя — такое правило считается «тем же», его не перетираем и не
// дублируем. Env-скоупленное пользовательское правило (Environment != "")
// ключом не является и НЕ блокирует создание all-env дефолта рецепта.
func matches(r metric.Rule, s RuleSpec) bool {
	return r.Environment == "" &&
		r.MetricName == s.Metric &&
		r.Aggregation == s.Agg &&
		r.Comparator == s.Comparator &&
		r.LabelKey == s.LabelKey &&
		r.LabelValue == s.LabelValue
}

// RuleStatuses возвращает статусы всех RuleSpec'ов рецепта в порядке
// r.Rules — по одному RuleStatus на спек.
func RuleStatuses(existing []metric.Rule, r Recipe) []RuleStatus {
	out := make([]RuleStatus, 0, len(r.Rules))
	for _, spec := range r.Rules {
		st := RuleStatus{Spec: spec}
		for _, ex := range existing {
			if matches(ex, spec) {
				st.Exists = true
				break
			}
		}
		out = append(out, st)
	}
	return out
}

// ApplyRules идемпотентно создаёт недостающие рекомендованные пороги рецепта
// как обычные metric alert rules (Environment="", Enabled=true). Возвращает
// (created, skipped). Идемпотентность — check-then-create поверх List: у
// metric_alert_rules НЕТ unique-констрейнта по ключу, так что гонка двойного
// клика теоретически даёт дубль правила — это benign (спека §4.4): дубль
// виден в списке правил и удаляется вручную, а повторный ApplyRules дублей
// уже не плодит. При частичном сбое (часть правил создана, затем Create
// упал) повторный вызов дозаполнит недостающие: уже созданные попадут в
// skipped, ошибка не оставляет проект в невосстановимом состоянии.
func ApplyRules(ctx context.Context, svc *metric.RuleService, projectID int64, r Recipe) (int, int, error) {
	existing, err := svc.List(ctx, projectID)
	if err != nil {
		return 0, 0, fmt.Errorf("recipes: apply %s: %w", r.ID, err)
	}
	created, skipped := 0, 0
	for _, st := range RuleStatuses(existing, r) {
		if st.Exists {
			skipped++
			continue
		}
		s := st.Spec
		if _, err := svc.Create(ctx, metric.Rule{
			ProjectID:     projectID,
			MetricName:    s.Metric,
			Aggregation:   s.Agg,
			Comparator:    s.Comparator,
			Threshold:     s.Threshold,
			WindowSeconds: s.WindowSeconds,
			LabelKey:      s.LabelKey,
			LabelValue:    s.LabelValue,
			Environment:   "",
			Enabled:       true,
			Severity:      s.Severity,
		}); err != nil {
			return created, skipped, fmt.Errorf("recipes: apply %s: create rule %s: %w", r.ID, s.Metric, err)
		}
		created++
	}
	return created, skipped, nil
}
