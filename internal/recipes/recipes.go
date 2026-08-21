// Package recipes — декларативный реестр «рецептов мониторинга» типовых
// сервисов (B6): postgres/nginx/redis/docker. Рецепт описывает всё, что нужно
// странице подключения сервиса: сигнатурную метрику для детекции «данные
// приходят», готовый YAML-конфиг otel-collector-contrib с ключом проекта,
// преднастроенные графики по известным именам метрик ресивера и
// рекомендованные пороги (обычные metric alert rules).
//
// Образец — internal/hostmetric: реестр имён как лист-пакет. Единственная
// внутренняя зависимость — internal/metric (тип LabelMatcher в ChartSeries);
// это осознанно и безопасно: metric не импортирует recipes, цикла нет
// (ревью плана MINOR-4 — локальный дубль LabelMatcher не заводить).
//
// Имена метрик, их типы (gauge / sum±monotonic) и datapoint- vs
// resource-природа атрибутов сверены с metadata.yaml ресиверов
// otel-collector-contrib (см. отчёт T1). Ключевой инвариант (BLOCKER-1
// спеки): наш ingest промотирует из resource-атрибутов только
// service.name/environment/host.name (metric.promote), остальные
// ВЫБРАСЫВАЕТ — поэтому resource-атрибуты, по которым группируют графики
// (container.name у docker, postgresql.database.name у postgres), конфиг
// рецепта ОБЯЗАН продвинуть transform-процессором в datapoint-атрибуты.
// Список таких атрибутов — PromotedAttrs; инвариант «каждый PromotedAttr
// присутствует в transform-сниппете Config» закреплён тестом реестра.
package recipes

import "gitflic.ru/otezvikentiy/gotcha/internal/metric"

// ChartSeries — один ряд преднастроенного графика. Половина графиков рецептов —
// пары разных метрик (commits+rollbacks, accepted+handled, hits+misses, rx+tx),
// одно поле Metric их не выражает (ревью MAJOR-1).
type ChartSeries struct {
	Metric string
	// Rate — ряд скоростей (monotonic cumulative sum): потребитель выбирает
	// SeriesGroupedRate для группированных рядов; одиночный Series сам
	// распознаёт monotonic cumulative и считает rate.
	Rate        bool
	Matchers    []metric.LabelMatcher // напр. state=active; ТОЛЬКО при GroupKey==""
	LabelSuffix string                // i18n-суффикс подписи ряда: recipes.<id>.series.<suffix>
}

// Chart — один преднастроенный график рецепта.
type Chart struct {
	Key    string // суффикс i18n-ключа заголовка: recipes.<id>.chart.<key>
	Unit   string // подсказка единицы оси (fallback к MetricInfo.Unit)
	Series []ChartSeries
	// GroupKey — группировка по datapoint-атрибуту (state, container.name…);
	// допустим ТОЛЬКО при ровно одной Series без Matchers (SeriesGrouped не
	// принимает matchers — ограничение модели). Инвариант дизайна: GroupKey
	// указывает на datapoint-атрибут ПОСЛЕ MapOTLP — либо родной атрибут
	// ресивера, либо продвинутый transform'ом (PromotedAttrs).
	GroupKey string
	Agg      string // из metric.Aggregations
}

// RuleSpec — рекомендованный порог; создаётся как ОБЫЧНОЕ metric alert rule
// (никаких новых сущностей БД). Для cumulative-метрик Aggregate делает rate,
// поэтому семантика порога на счётчик — «прирост за окно», не «количество»
// (NoteKey обязан формулировать именно так, ревью MINOR-2).
type RuleSpec struct {
	Metric, Agg, Comparator string // comparator: gt|lt
	Threshold               float64
	WindowSeconds           int
	LabelKey, LabelValue    string // опциональный матчер правила (nginx state=active)
	Severity                string // "" (=дефолт warning) | "critical"
	NoteKey                 string // i18n-суффикс пояснения: recipes.<id>.rule.<notekey>
}

// Recipe — один сервис-рецепт: всё, что нужно странице подключения.
type Recipe struct {
	ID string // slug: postgres|nginx|redis|docker
	// Signature — метрика для детекции «данные приходят». Всегда метрика,
	// которую наш тракт агрегирует скалярно с первого же скрейпа: gauge или
	// НЕ-monotonic sum (monotonic cumulative через aggregateRate требует ≥2
	// корзин — статус загорался бы только со второго скрейпа, ревью MINOR-3).
	Signature string
	Metrics   []string // все известные имена (справка + ссылки в explorer)
	// PromotedAttrs — resource-атрибуты, которые сниппет Config ОБЯЗАН
	// продвинуть transform'ом в datapoint-атрибуты (см. package doc).
	PromotedAttrs []string
	Charts        []Chart
	Rules         []RuleSpec // может быть пуст (docker — пер-контейнерные
	// метрики, разумного общего дефолта нет; честно помечается на странице)
	Config func(baseURL, apiKey string) string
}

// All возвращает рецепты в порядке показа.
func All() []Recipe { return registry }

// ByID находит рецепт по slug'у.
func ByID(id string) (Recipe, bool) {
	for _, r := range registry {
		if r.ID == id {
			return r, true
		}
	}
	return Recipe{}, false
}
