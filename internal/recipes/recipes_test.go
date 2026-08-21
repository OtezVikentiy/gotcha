package recipes_test

import (
	"math"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
)

// validAgg — проверка агрегации по metric.Aggregations (источник истины
// rule.go): реестр не имеет права ссылаться на агрегацию, которую движок
// правил и explorer не понимают.
func validAgg(a string) bool {
	for _, x := range metric.Aggregations {
		if x == a {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// nativeDatapointAttrs — родные datapoint-атрибуты ресиверов по итогам сверки
// с metadata.yaml otel-collector-contrib (T1 Step 1): nginx.connections_current
// несёт `state` (active/reading/writing/waiting) прямо на датапойнте;
// postgresql.rows — `state` (dead/live). Для них transform не нужен — атрибут
// доезжает до attributes через MapOTLP без продвижения.
func nativeDatapointAttrs(id string) []string {
	switch id {
	case "nginx":
		return []string{"state"}
	case "postgres":
		return []string{"state"}
	}
	return nil
}

func TestRegistryInvariants(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range recipes.All() {
		if r.ID == "" || r.Signature == "" || len(r.Charts) == 0 || r.Config == nil {
			t.Fatalf("recipe %q: пустые обязательные поля", r.ID)
		}
		if seen[r.ID] {
			t.Fatalf("дубль ID %q", r.ID)
		}
		seen[r.ID] = true
		if !contains(r.Metrics, r.Signature) {
			t.Fatalf("recipe %q: Signature не в Metrics", r.ID)
		}
		chartKeys := map[string]bool{}
		for _, c := range r.Charts {
			// Chart.Key — суффикс i18n-ключа заголовка и data-chart-маркер:
			// дубль дал бы два графика с одним заголовком и неразличимые тесты.
			if chartKeys[c.Key] {
				t.Fatalf("recipe %q: дубль Chart.Key %q", r.ID, c.Key)
			}
			chartKeys[c.Key] = true
			if c.GroupKey != "" {
				if len(c.Series) != 1 || len(c.Series[0].Matchers) != 0 {
					t.Fatalf("recipe %q chart %q: GroupKey требует ровно 1 Series без Matchers", r.ID, c.Key)
				}
			}
			if len(c.Series) == 0 {
				t.Fatalf("recipe %q chart %q: нет рядов", r.ID, c.Key)
			}
			// Пара рядов без различимых LabelSuffix — легенда из двух
			// одинаковых (или пустых) подписей, нечитаемая по построению.
			if len(c.Series) == 2 {
				a, b := c.Series[0].LabelSuffix, c.Series[1].LabelSuffix
				if a == "" || b == "" || a == b {
					t.Fatalf("recipe %q chart %q: у пары рядов LabelSuffix должны быть непустыми и различными (%q, %q)", r.ID, c.Key, a, b)
				}
			}
			if !validAgg(c.Agg) {
				t.Fatalf("recipe %q chart %q: agg %q", r.ID, c.Key, c.Agg)
			}
			for _, s := range c.Series {
				if s.Metric == "" || !contains(r.Metrics, s.Metric) {
					t.Fatalf("recipe %q chart %q: метрика ряда %q не в Metrics", r.ID, c.Key, s.Metric)
				}
			}
		}
		cfg := r.Config("https://gotcha.example", "test-key-123")
		if !strings.Contains(cfg, "https://gotcha.example") || !strings.Contains(cfg, "test-key-123") {
			t.Fatalf("recipe %q: Config без endpoint/ключа", r.ID)
		}
		if strings.Contains(cfg, "%!") {
			t.Fatalf("recipe %q: артефакт форматирования в Config", r.ID)
		}
		// Transform-инвариант (BLOCKER-1 спеки): каждый PromotedAttr реально
		// продвигается сниппетом ПОЛНЫМ transform-стейтментом (два раздельных
		// Contains пропускали бы attr, упомянутый где угодно в конфиге при
		// любом transform); GroupKey либо продвинут, либо родной
		// datapoint-атрибут (nginx/postgres state — сверено в Step 1).
		for _, attr := range r.PromotedAttrs {
			stmt := `set(attributes["` + attr + `"], resource.attributes["` + attr + `"])`
			if !strings.Contains(cfg, stmt) {
				t.Fatalf("recipe %q: PromotedAttr %q не продвинут transform'ом в Config (нет %q)", r.ID, attr, stmt)
			}
		}
		for _, c := range r.Charts {
			if c.GroupKey != "" && !contains(r.PromotedAttrs, c.GroupKey) && !contains(nativeDatapointAttrs(r.ID), c.GroupKey) {
				t.Fatalf("recipe %q chart %q: GroupKey %q ни продвинут, ни родной datapoint-атрибут", r.ID, c.Key, c.GroupKey)
			}
		}
		ruleKeys := map[string]bool{}
		for _, rs := range r.Rules {
			if !validAgg(rs.Agg) || (rs.Comparator != "gt" && rs.Comparator != "lt") {
				t.Fatalf("recipe %q rule %q: agg/comparator", r.ID, rs.Metric)
			}
			if rs.Severity != "" && rs.Severity != "critical" {
				t.Fatalf("recipe %q rule %q: severity %q", r.ID, rs.Metric, rs.Severity)
			}
			if rs.WindowSeconds <= 0 || math.IsNaN(rs.Threshold) || math.IsInf(rs.Threshold, 0) {
				t.Fatalf("recipe %q rule %q: window/threshold", r.ID, rs.Metric)
			}
			if !contains(r.Metrics, rs.Metric) {
				t.Fatalf("recipe %q rule: метрика %q не в Metrics", r.ID, rs.Metric)
			}
			// LabelValue без LabelKey ключ идемпотентности (matches) ещё
			// переживёт, а LabelKey без LabelValue — матчер «ключ есть,
			// значение пустое», которого модель правил не выражает.
			if rs.LabelKey != "" && rs.LabelValue == "" {
				t.Fatalf("recipe %q rule %q: LabelKey без LabelValue", r.ID, rs.Metric)
			}
			// Полный ключ идемпотентности ApplyRules: два RuleSpec с одним
			// ключом в одном рецепте — второй никогда не создастся (первый
			// уже existing) и вечно висел бы «будет создан».
			key := rs.Metric + "|" + rs.Agg + "|" + rs.Comparator + "|" + rs.LabelKey + "|" + rs.LabelValue
			if ruleKeys[key] {
				t.Fatalf("recipe %q: дубль ключа RuleSpec %q", r.ID, key)
			}
			ruleKeys[key] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("ожидалось 4 рецепта, есть %d", len(seen))
	}
}

func TestByID(t *testing.T) {
	r, ok := recipes.ByID("postgres")
	if !ok || r.ID != "postgres" {
		t.Fatalf("ByID(postgres): ok=%v id=%q", ok, r.ID)
	}
	if _, ok := recipes.ByID("nope"); ok {
		t.Fatalf("ByID(nope): ожидалось !ok")
	}
}
