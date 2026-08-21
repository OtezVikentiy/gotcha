package web

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// recipeChartWindow — ФИКСИРОВАННОЕ окно преднастроенных графиков рецепта:
// последние 24 часа (спека B6 F6, ревью плана MINOR-1). Нарочно НЕ
// resolveTimeRange: тот читает глобальную куку диапазона, и страница рецепта
// молча уезжала бы за выбором, сделанным на экранах метрик/хостов, — а здесь
// нет ни селектора периода, ни намёка, что период вообще настраивается.
const recipeChartWindow = 24 * time.Hour

// recipeCharts строит VM преднастроенных графиков рецепта — по одному на
// КАЖДЫЙ Chart реестра, в его порядке. Ошибка query одного графика — его
// Empty с логом, не 500 всей страницы (в отличие от hostDetailCharts:
// карточка хоста — основной экран телеметрии, а графики рецепта —
// вспомогательный блок страницы подключения, которая обязана работать и при
// хромающей аналитике — тот же принцип, что recipeDataArrives).
//
// host везде пустой: метрики рецептов приходят без resourcedetection (реестр,
// спека §5 MAJOR-2), пустой-байпас host в query-слое (T2) означает «все хосты».
func (h *Handler) recipeCharts(ctx context.Context, projectID int64, rec recipes.Recipe, from, to time.Time, step time.Duration) []templates.RecipeChartVM {
	// Маркеры деплоев (C5) — один дозапрос на все графики рецепта, как у
	// hostDetailCharts; nil-guard — стенды без деплоев маркеров не рисуют.
	var deploys []deploy.Deployment
	if h.Deploy != nil {
		deploys, _ = h.Deploy.List(ctx, projectID, from, to, 20)
	}
	out := make([]templates.RecipeChartVM, 0, len(rec.Charts))
	for _, chart := range rec.Charts {
		out = append(out, h.recipeChartVM(ctx, projectID, rec, chart, deploys, from, to, step))
	}
	return out
}

// recipeChartVM — один график рецепта: групповая ветка (SeriesGrouped /
// SeriesGroupedRate по Chart.GroupKey) или одиночные/парные ряды (Series на
// каждый ChartSeries, собранные в общий multiSeriesSVG).
func (h *Handler) recipeChartVM(ctx context.Context, projectID int64, rec recipes.Recipe, chart recipes.Chart, deploys []deploy.Deployment, from, to time.Time, step time.Duration) templates.RecipeChartVM {
	vm := templates.RecipeChartVM{
		Key:      chart.Key,
		TitleKey: "recipes." + rec.ID + ".chart." + chart.Key,
	}
	// Реестр гарантирует непустые Series (инвариант-тест), но билдер зовётся
	// и с синтетическими рецептами: без гварда пустой Chart уронил бы
	// Series[0] (здесь же, в recipeExplorerURL) паникой, а не честным Empty.
	if len(chart.Series) == 0 {
		vm.Empty = true
		return vm
	}
	vm.ExplorerURL = recipeExplorerURL(projectID, chart)
	var series []NamedSeries
	var legend []templates.LegendItem
	if chart.GroupKey != "" {
		// Инвариант модели (T1): GroupKey ⇒ ровно одна Series без Matchers.
		s := chart.Series[0]
		var result metric.GroupedSeriesResult
		var err error
		if s.Rate {
			// deviceKey="" ОСОЗНАННО (ревью плана MAJOR-1): SeriesGroupedRate
			// считает rate на размерности (groupKey, deviceKey), и пустой
			// deviceKey схлопывает мелкую размерность — rate считается прямо на
			// GroupKey. Это корректно, когда GroupKey и есть самый мелкий
			// источник счётчика (база у postgres, контейнер у docker) — в
			// отличие от хостов, где direction агрегирует счётчики РАЗНЫХ
			// device и deviceKey обязателен. Chart.Agg здесь НЕ участвует:
			// метод его не принимает (rate — не скалярная агрегация по бакету).
			result, err = h.Metrics.SeriesGroupedRate(ctx, projectID, s.Metric, "", chart.GroupKey, "", from, to, step)
		} else {
			result, err = h.Metrics.SeriesGrouped(ctx, projectID, s.Metric, "", chart.GroupKey, chart.Agg, from, to, step)
		}
		if err != nil {
			slog.Warn("recipes: chart query failed", "project_id", projectID, "recipe", rec.ID, "chart", chart.Key, "error", err)
			vm.Empty = true
			return vm
		}
		if len(result.Groups) == 0 {
			vm.Empty = true
			return vm
		}
		// Легенда групповых рядов — сырые ключи групп (имя базы, контейнера):
		// это открытые множества значений, i18n-карта для них невозможна —
		// та же логика, что mountpoint у графика диска хоста.
		series, legend = namedSeriesFromGroups(result.Groups, 1, from, to, step)
		vm.Truncated = result.Truncated
	} else {
		// Одиночные и парные ряды: Series сам делает rate для monotonic
		// cumulative (Rate у ChartSeries — подсказка для групповой ветки;
		// здесь тип определяется по данным). Empty — только когда пусты ВСЕ
		// ряды: отсутствующая половина пары рисуется NaN-разрывом, не гасит
		// график целиком (прецедент hostLoadChart).
		empty := true
		for i, s := range chart.Series {
			pts, err := h.Metrics.Series(ctx, projectID, s.Metric, "", "", s.Matchers, chart.Agg, from, to, step)
			if err != nil {
				slog.Warn("recipes: chart query failed", "project_id", projectID, "recipe", rec.ID, "chart", chart.Key, "error", err)
				vm.Empty = true
				return vm
			}
			if len(pts) > 0 {
				empty = false
			}
			label := recipeSeriesLabel(ctx, rec, chart, s)
			series = append(series, NamedSeries{Label: label, Points: hostGapFill(pts, from, to, step)})
			legend = append(legend, templates.LegendItem{Label: label, Class: "legend-m" + strconv.Itoa(i+1)})
		}
		if empty {
			vm.Empty = true
			return vm
		}
	}
	vm.Chart = multiSeriesSVG(ctx, series, h.recipeChartUnit(ctx, projectID, chart), nil, deploys, hostChartWidth, hostChartHeight)
	vm.Legend = legend
	return vm
}

// recipeChartUnit — единица оси графика: Chart.Unit реестра, при пустом —
// Unit метрики из ingest (документированный контракт Chart.Unit: «fallback к
// MetricInfo.Unit»). Ошибка/отсутствие метрики — просто без единицы, график
// важнее подписи (тот же принцип, что metricThresholdsFor); безразмерное "1"
// formatAxisValue и так не печатает. Дозапрос только для графиков без Unit в
// реестре и только когда данные есть (Empty-ветки выходят раньше).
func (h *Handler) recipeChartUnit(ctx context.Context, projectID int64, chart recipes.Chart) string {
	if chart.Unit != "" {
		return chart.Unit
	}
	info, found, err := h.Metrics.MetricInfoByName(ctx, projectID, chart.Series[0].Metric)
	if err != nil || !found {
		return ""
	}
	return info.Unit
}

// recipeSeriesLabel — подпись ряда: для пар — i18n по LabelSuffix
// (recipes.<id>.series.<suffix>, ключи завёл T4), для одиночного ряда без
// суффикса — заголовок графика (как hostCPUChart: легенда из одного пункта
// повторяет заголовок, но даёт свотч цвета линии).
func recipeSeriesLabel(ctx context.Context, rec recipes.Recipe, chart recipes.Chart, s recipes.ChartSeries) string {
	if s.LabelSuffix != "" {
		return i18n.T(ctx, "recipes."+rec.ID+".series."+s.LabelSuffix)
	}
	return i18n.T(ctx, "recipes."+rec.ID+".chart."+chart.Key)
}

// recipeExplorerURL — ссылка «открыть в метриках»: страница первой метрики
// графика с его агрегацией (формат ?agg= — как читает metricDetail).
func recipeExplorerURL(projectID int64, chart recipes.Chart) string {
	u := metricDetailURL(projectID, chart.Series[0].Metric)
	if chart.Agg != "" {
		u += "?agg=" + url.QueryEscape(chart.Agg)
	}
	return u
}
