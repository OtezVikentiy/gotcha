package recipes_test

import (
	"math"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
)

// сверяет с metric.Aggregations — источником истины для движка правил и explorer.
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

// родные datapoint-атрибуты ресиверов (сверено с metadata.yaml) — для них transform не нужен.
func nativeDatapointAttrs(id string) []string {
	switch id {
	case "nginx":
		return []string{"state"}
	case "postgres":
		return []string{"state"}
	case "mariadb":
		return []string{"kind", "operation"}
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
			// порог >=2, не >2: у mariadb threads уже три ряда, легенда неразличима с двух.
			if len(c.Series) >= 2 {
				suffixes := map[string]bool{}
				for _, s := range c.Series {
					if s.LabelSuffix == "" || suffixes[s.LabelSuffix] {
						t.Fatalf("recipe %q chart %q: LabelSuffix рядов должны быть непустыми и попарно различными (%q)", r.ID, c.Key, s.LabelSuffix)
					}
					suffixes[s.LabelSuffix] = true
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
		// YAML-парсер, не strings.Contains: битый отступ пережил бы проверку строк,
		// но сломал бы коллектор при реальной загрузке сниппета пользователем.
		var parsed struct {
			Receivers map[string]any `yaml:"receivers"`
			Exporters map[string]any `yaml:"exporters"`
			Service   struct {
				Pipelines map[string]any `yaml:"pipelines"`
			} `yaml:"service"`
		}
		if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
			t.Fatalf("recipe %q: Config не разбирается как YAML: %v\n%s", r.ID, err, cfg)
		}
		if len(parsed.Receivers) == 0 {
			t.Fatalf("recipe %q: Config без секции receivers после разбора YAML\n%s", r.ID, cfg)
		}
		if len(parsed.Exporters) == 0 {
			t.Fatalf("recipe %q: Config без секции exporters после разбора YAML\n%s", r.ID, cfg)
		}
		if len(parsed.Service.Pipelines) == 0 {
			t.Fatalf("recipe %q: Config без секции service.pipelines после разбора YAML\n%s", r.ID, cfg)
		}
		// целым stmt, не двумя раздельными Contains: те пропускали бы attr,
		// упомянутый где угодно в конфиге при любом transform.
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
			// LabelKey без LabelValue — матчер «ключ есть, значение пустое», которого
			// модель правил не выражает (обратное, LabelValue без LabelKey, допустимо).
			if rs.LabelKey != "" && rs.LabelValue == "" {
				t.Fatalf("recipe %q rule %q: LabelKey без LabelValue", r.ID, rs.Metric)
			}
			// два RuleSpec с одним ключом ApplyRules в одном рецепте — второй
			// никогда не создастся, первый навсегда числится existing.
			key := rs.Metric + "|" + rs.Agg + "|" + rs.Comparator + "|" + rs.LabelKey + "|" + rs.LabelValue
			if ruleKeys[key] {
				t.Fatalf("recipe %q: дубль ключа RuleSpec %q", r.ID, key)
			}
			ruleKeys[key] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("ожидалось 5 рецептов, есть %d", len(seen))
	}
}

// databases закомментированным даёт ресиверу все базы сразу — deadlocks-порог
// (max() по бакету) тогда слеп к всплеску в любой не самой крупной базе.
func TestPostgresConfigScopesDatabases(t *testing.T) {
	r, ok := recipes.ByID("postgres")
	if !ok {
		t.Fatal("ByID(postgres): !ok")
	}
	cfg := r.Config("https://gotcha.example", "test-key-123")
	if !strings.Contains(cfg, "\n    databases: [CHANGE_ME]") {
		t.Fatalf("Config: databases не задан явно (незакомментированным списком):\n%s", cfg)
	}
	if strings.Contains(cfg, "# databases") {
		t.Fatalf("Config: databases всё ещё закомментирован — ресивер возьмёт все базы:\n%s", cfg)
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
