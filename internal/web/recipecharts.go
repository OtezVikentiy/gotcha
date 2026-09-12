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

// Намеренно не resolveTimeRange: та читает глобальную куку диапазона, и график рецепта
// молча уехал бы за выбором с других экранов, хотя здесь нет селектора периода.
const recipeChartWindow = 24 * time.Hour

// Ошибка запроса одного графика — его Empty с логом, не 500 всей страницы: графики рецепта —
// вспомогательный блок, обязанный работать и при хромающей аналитике.
func (h *Handler) recipeCharts(ctx context.Context, projectID int64, rec recipes.Recipe, from, to time.Time, step time.Duration) []templates.RecipeChartVM {
	// nil-guard: стенды без Deploy не рисуют маркеры деплоев.
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

func (h *Handler) recipeChartVM(ctx context.Context, projectID int64, rec recipes.Recipe, chart recipes.Chart, deploys []deploy.Deployment, from, to time.Time, step time.Duration) templates.RecipeChartVM {
	vm := templates.RecipeChartVM{
		Key:      chart.Key,
		TitleKey: "recipes." + rec.ID + ".chart." + chart.Key,
	}
	// Билдер зовётся и с синтетическими рецептами — без гварда пустой Chart уронил бы
	// Series[0] дальше паникой, не Empty.
	if len(chart.Series) == 0 {
		vm.Empty = true
		return vm
	}
	vm.ExplorerURL = recipeExplorerURL(projectID, chart)
	var series []NamedSeries
	var legend []templates.LegendItem
	if chart.GroupKey != "" {
		// Инвариант модели: GroupKey ⇒ ровно одна Series без Matchers.
		s := chart.Series[0]
		var result metric.GroupedSeriesResult
		var err error
		if s.Rate {
			// deviceKey="" осознанно: rate считается прямо на GroupKey, корректно когда GroupKey
			// сам — самый мелкий источник счётчика (в отличие от хостов, где нужен deviceKey).
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
		// Легенда групповых рядов — сырые ключи групп: открытое множество значений, i18n-карта
		// для них невозможна (та же логика, что mountpoint диска хоста).
		series, legend = namedSeriesFromGroups(result.Groups, 1, from, to, step)
		vm.Truncated = result.Truncated
	} else {
		// Empty — только когда пусты ВСЕ ряды: отсутствующая половина пары рисуется
		// NaN-разрывом, не гасит график целиком.
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

// Единица — Chart.Unit реестра, при пустом fallback к Unit метрики из ingest; ошибка/нет
// метрики — без единицы, график важнее подписи.
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

// Без суффикса — заголовок графика: легенда из одного пункта, но со свотчем цвета линии.
func recipeSeriesLabel(ctx context.Context, rec recipes.Recipe, chart recipes.Chart, s recipes.ChartSeries) string {
	if s.LabelSuffix != "" {
		return i18n.T(ctx, "recipes."+rec.ID+".series."+s.LabelSuffix)
	}
	return i18n.T(ctx, "recipes."+rec.ID+".chart."+chart.Key)
}

func recipeExplorerURL(projectID int64, chart recipes.Chart) string {
	u := metricDetailURL(projectID, chart.Series[0].Metric)
	if chart.Agg != "" {
		u += "?agg=" + url.QueryEscape(chart.Agg)
	}
	return u
}
