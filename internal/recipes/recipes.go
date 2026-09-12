package recipes

import "gitflic.ru/otezvikentiy/gotcha/internal/metric"

type ChartSeries struct {
	Metric string
	// ряд скоростей (monotonic cumulative sum): группированные ряды считает
	// SeriesGroupedRate, одиночный Series распознаёт monotonic cumulative сам.
	Rate        bool
	Matchers    []metric.LabelMatcher // напр. state=active; ТОЛЬКО при GroupKey==""
	LabelSuffix string                // i18n-суффикс подписи ряда: recipes.<id>.series.<suffix>
}

type Chart struct {
	Key    string // суффикс i18n-ключа заголовка: recipes.<id>.chart.<key>
	Unit   string // подсказка единицы оси (fallback к MetricInfo.Unit)
	Series []ChartSeries
	// Допустим только при одной Series без Matchers; указывает на datapoint-атрибут после
	// MapOTLP — родной у ресивера либо продвинутый transform'ом (PromotedAttrs).
	GroupKey string
	Agg      string // из metric.Aggregations
}

// для cumulative-метрик Aggregate делает rate, поэтому семантика порога на
// счётчик — «прирост за окно», не «количество» (NoteKey должен так и говорить).
type RuleSpec struct {
	Metric, Agg, Comparator string // comparator: gt|lt
	Threshold               float64
	WindowSeconds           int
	LabelKey, LabelValue    string // опциональный матчер правила (nginx state=active)
	Severity                string // "" (=дефолт warning) | "critical"
	NoteKey                 string // i18n-суффикс пояснения: recipes.<id>.rule.<notekey>
}

type Recipe struct {
	ID string // slug: postgres|nginx|redis|docker
	// gauge или НЕ-monotonic sum: monotonic cumulative через aggregateRate
	// требует ≥2 корзин, статус загорался бы только со второго скрейпа.
	Signature string
	Metrics   []string // все известные имена (справка + ссылки в explorer)
	// ingest промотирует из resource-атрибутов только service.name/environment/host.name,
	// остальные выбрасывает — эти сниппет Config обязан продвинуть transform'ом сам.
	PromotedAttrs []string
	Charts        []Chart
	Rules         []RuleSpec // может быть пуст (docker — пер-контейнерные
	// метрики, разумного общего дефолта нет; честно помечается на странице)
	Config func(baseURL, apiKey string) string
}

func All() []Recipe { return registry }

func ByID(id string) (Recipe, bool) {
	for _, r := range registry {
		if r.ID == id {
			return r, true
		}
	}
	return Recipe{}, false
}
