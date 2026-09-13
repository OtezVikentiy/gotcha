package recipes

import (
	"context"
	"fmt"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

type RuleStatus struct {
	Spec   RuleSpec
	Exists bool
	// Валиден только при Exists: правило может существовать выключенным —
	// «Создан» тогда неверно отвечает на вопрос «у меня настроены пороги?».
	Enabled bool
}

// вне ключа — Threshold, WindowSeconds, Enabled: иначе подстроенный порог или
// отключённое правило задваивались бы; Environment != "" не в счёт и не блокирует all-env дефолт.
func matches(r metric.Rule, s RuleSpec) bool {
	return r.Environment == "" &&
		r.MetricName == s.Metric &&
		r.Aggregation == s.Agg &&
		r.Comparator == s.Comparator &&
		r.LabelKey == s.LabelKey &&
		r.LabelValue == s.LabelValue
}

func RuleStatuses(existing []metric.Rule, r Recipe) []RuleStatus {
	out := make([]RuleStatus, 0, len(r.Rules))
	for _, spec := range r.Rules {
		st := RuleStatus{Spec: spec}
		for _, ex := range existing {
			if matches(ex, spec) {
				st.Exists = true
				st.Enabled = ex.Enabled
				break
			}
		}
		out = append(out, st)
	}
	return out
}

// check-then-create поверх List: unique-констрейнта на ключ нет, гонка двойного
// клика может дать дубль правила — терпимо, дубль виден в списке и удаляется вручную.
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
