package web

import (
	"context"
	"math"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Аспект 960×210 шире квадратного графика метрики: графики хоста стоят в столбец во всю ширину.
const (
	hostChartWidth  = 960
	hostChartHeight = 210
)

// Карта, не конкатенация "hosts.legend."+key: набор значений открыт снизу (зависит от версии
// коллектора/ОС) — незнакомое значение обязано остаться как есть, а не сырым i18n-ключом.
var hostGroupLabelKeys = map[string]string{
	"read":     "hosts.legend.read",
	"write":    "hosts.legend.write",
	"receive":  "hosts.legend.receive",
	"transmit": "hosts.legend.transmit",
	"running":  "hosts.legend.running",
	"sleeping": "hosts.legend.sleeping",
	"idle":     "hosts.legend.idle",
	"stopped":  "hosts.legend.stopped",
	"zombies":  "hosts.legend.zombies",
	"paging":   "hosts.legend.paging",
	"blocked":  "hosts.legend.blocked",
	"daemon":   "hosts.legend.daemon",
	"orphan":   "hosts.legend.orphan",
	"system":   "hosts.legend.system",
	"unknown":  "hosts.legend.unknown",
}

// series/legend — параллельные срезы по построению (namedSeriesFromGroups из одного g.Key);
// проверка длин страхует от будущего расхождения индексов.
func localizeGroupLabels(ctx context.Context, series []NamedSeries, legend []templates.LegendItem) {
	if len(series) != len(legend) {
		return
	}
	for i := range series {
		key, ok := hostGroupLabelKeys[series[i].Label]
		if !ok {
			continue
		}
		label := i18n.T(ctx, key)
		series[i].Label = label
		legend[i].Label = label
	}
}

func hostGapFill(points []metric.Point, from, to time.Time, step time.Duration) []metric.Point {
	return fillSeries(points, from, to, step,
		func(p metric.Point) time.Time { return p.T },
		func(t time.Time) metric.Point { return metric.Point{T: t, V: math.NaN()} })
}

// Ошибка любого графика — общая 500: частичная карточка хуже честной ошибки.
func (h *Handler) hostDetailCharts(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings) ([]templates.HostChartVM, error) {
	// Один дозапрос выкладок на все семь графиков; nil-guard — стенды без деплоев маркеров не рисуют.
	var deploys []deploy.Deployment
	if h.Deploy != nil {
		deploys, _ = h.Deploy.List(ctx, projectID, from, to, 20)
	}
	cpu, err := h.hostCPUChart(ctx, projectID, name, from, to, step, deploys)
	if err != nil {
		return nil, err
	}
	mem, err := h.hostMemChart(ctx, projectID, name, from, to, step, settings, deploys)
	if err != nil {
		return nil, err
	}
	diskUsage, err := h.hostDiskUsageChart(ctx, projectID, name, from, to, step, settings, deploys)
	if err != nil {
		return nil, err
	}
	diskIO, err := h.hostRateChart(ctx, projectID, hostmetric.DiskIO, name, from, to, step, "disk_io", deploys)
	if err != nil {
		return nil, err
	}
	net, err := h.hostRateChart(ctx, projectID, hostmetric.NetworkIO, name, from, to, step, "net", deploys)
	if err != nil {
		return nil, err
	}
	load, err := h.hostLoadChart(ctx, projectID, name, from, to, step, settings, deploys)
	if err != nil {
		return nil, err
	}
	proc, err := h.hostProcChart(ctx, projectID, name, from, to, step, deploys)
	if err != nil {
		return nil, err
	}
	return []templates.HostChartVM{cpu, mem, diskUsage, diskIO, net, load, proc}, nil
}

func (h *Handler) hostCPUChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	pts, err := h.Metrics.Series(ctx, projectID, hostmetric.CPUUtilization, "", name,
		[]metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "idle"}}, "avg", from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(pts) == 0 {
		return templates.HostChartVM{Key: "cpu", Empty: true}, nil
	}
	busy := make([]metric.Point, len(pts))
	for i, p := range pts {
		busy[i] = metric.Point{T: p.T, V: (1 - p.V) * 100}
	}
	series := []NamedSeries{{Label: i18n.T(ctx, "hosts.chart.cpu"), Points: hostGapFill(busy, from, to, step)}}
	return templates.HostChartVM{
		Key:    "cpu",
		Chart:  multiSeriesSVG(ctx, series, "%", nil, deploys, hostChartWidth, hostChartHeight),
		Legend: []templates.LegendItem{{Label: i18n.T(ctx, "hosts.chart.cpu"), Class: "legend-m1"}},
	}, nil
}

// Значения хранятся долями [0,1] (как везде в host-пороговой подсистеме), на графике — проценты.
// Порог рисуется только если включён — линия выключенного порога вводила бы в заблуждение.
func (h *Handler) hostMemChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	pts, err := h.Metrics.Series(ctx, projectID, hostmetric.MemoryUtilization, "", name,
		[]metric.LabelMatcher{{Key: hostmetric.AttrState, Value: "used"}}, "avg", from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(pts) == 0 {
		return templates.HostChartVM{Key: "mem", Empty: true}, nil
	}
	scaled := make([]metric.Point, len(pts))
	for i, p := range pts {
		scaled[i] = metric.Point{T: p.T, V: p.V * 100}
	}
	var thresholds []metricThreshold
	if settings.MemoryEnabled {
		thresholds = append(thresholds, metricThreshold{Value: settings.MemoryThreshold * 100, Comparator: "gt"})
	}
	series := []NamedSeries{{Label: i18n.T(ctx, "hosts.chart.mem"), Points: hostGapFill(scaled, from, to, step)}}
	return templates.HostChartVM{
		Key:    "mem",
		Chart:  multiSeriesSVG(ctx, series, "%", thresholds, deploys, hostChartWidth, hostChartHeight),
		Legend: []templates.LegendItem{{Label: i18n.T(ctx, "hosts.chart.mem"), Class: "legend-m1"}},
	}, nil
}

func (h *Handler) hostDiskUsageChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	result, err := h.Metrics.SeriesGrouped(ctx, projectID, hostmetric.FilesystemUtilization, name, hostmetric.AttrMountpoint, "avg", from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(result.Groups) == 0 {
		return templates.HostChartVM{Key: "disk_usage", Empty: true}, nil
	}
	series, legend := namedSeriesFromGroups(result.Groups, 100, from, to, step)
	var thresholds []metricThreshold
	if settings.DiskEnabled {
		thresholds = append(thresholds, metricThreshold{Value: settings.DiskThreshold * 100, Comparator: "gt"})
	}
	return templates.HostChartVM{
		Key:       "disk_usage",
		Chart:     multiSeriesSVG(ctx, series, "%", thresholds, deploys, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

func (h *Handler) hostRateChart(ctx context.Context, projectID int64, metricName, hostName string, from, to time.Time, step time.Duration, key string, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	result, err := h.Metrics.SeriesGroupedRate(ctx, projectID, metricName, hostName, hostmetric.AttrDirection, hostmetric.AttrDevice, from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(result.Groups) == 0 {
		return templates.HostChartVM{Key: key, Empty: true}, nil
	}
	series, legend := namedSeriesFromGroups(result.Groups, 1, from, to, step)
	localizeGroupLabels(ctx, series, legend)
	// Байты в секунду для disk.io и network.io — без явного юнита ось печатала голое число, и
	// читатель не мог отличить байты от пакетов.
	return templates.HostChartVM{
		Key:       key,
		Chart:     multiSeriesSVG(ctx, series, i18n.T(ctx, "hosts.chart.unit.bytes_per_second"), nil, deploys, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

// Порог делится на ЧИСЛО ЯДЕР СЕЙЧАС (LatestByHost), не на период графика. Пусто — только
// если ВСЕ три ряда пусты: пропавшая линия рисуется NaN-разрывом, не гасит весь график.
func (h *Handler) hostLoadChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	names := []string{hostmetric.LoadAvg1m, hostmetric.LoadAvg5m, hostmetric.LoadAvg15m}
	labels := []string{"1m", "5m", "15m"}
	raw := make([][]metric.Point, len(names))
	empty := true
	for i, n := range names {
		pts, err := h.Metrics.Series(ctx, projectID, n, "", name, nil, "avg", from, to, step)
		if err != nil {
			return templates.HostChartVM{}, err
		}
		if len(pts) > 0 {
			empty = false
		}
		raw[i] = pts
	}
	if empty {
		return templates.HostChartVM{Key: "load", Empty: true}, nil
	}
	series, legend := hostLoadSeries(raw, labels, from, to, step)
	var thresholds []metricThreshold
	if settings.LoadEnabled {
		cores, ok, err := h.hostCores(ctx, projectID, name)
		if err != nil {
			return templates.HostChartVM{}, err
		}
		if ok && cores > 0 {
			thresholds = append(thresholds, metricThreshold{Value: cores * settings.LoadThreshold, Comparator: "gt"})
		}
	}
	return templates.HostChartVM{
		Key:    "load",
		Chart:  multiSeriesSVG(ctx, series, "", thresholds, deploys, hostChartWidth, hostChartHeight),
		Legend: legend,
	}, nil
}

// Отдельная функция — единственное место, где виден частичный отказ рядов (не все три
// метрики отданы коллектором), и его можно проверить без ClickHouse.
func hostLoadSeries(raw [][]metric.Point, labels []string, from, to time.Time, step time.Duration) ([]NamedSeries, []templates.LegendItem) {
	series := make([]NamedSeries, len(labels))
	legend := make([]templates.LegendItem, len(labels))
	for i := range labels {
		series[i] = NamedSeries{Label: labels[i], Points: hostGapFill(raw[i], from, to, step)}
		legend[i] = templates.LegendItem{Label: labels[i], Class: "legend-m" + strconv.Itoa(i+1)}
	}
	return series, legend
}

// Окно от РЕАЛЬНОГО «сейчас», не от периода графика: делитель порога load должен быть
// текущим числом ядер.
func (h *Handler) hostCores(ctx context.Context, projectID int64, name string) (float64, bool, error) {
	now := time.Now()
	byHost, err := h.Metrics.LatestByHost(ctx, projectID, hostmetric.CPULogicalCount, nil, "", "", now.Add(-hostsListWindow), now)
	if err != nil {
		return 0, false, err
	}
	v, ok := byHost[name]
	return v, ok, nil
}

func (h *Handler) hostProcChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, deploys []deploy.Deployment) (templates.HostChartVM, error) {
	result, err := h.Metrics.SeriesGrouped(ctx, projectID, hostmetric.ProcessesCount, name, hostmetric.AttrStatus, "avg", from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(result.Groups) == 0 {
		return templates.HostChartVM{Key: "proc", Empty: true}, nil
	}
	series, legend := namedSeriesFromGroups(result.Groups, 1, from, to, step)
	localizeGroupLabels(ctx, series, legend)
	return templates.HostChartVM{
		Key:       "proc",
		Chart:     multiSeriesSVG(ctx, series, "", nil, deploys, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

// Дозаполнение ОБЯЗАТЕЛЬНО, не косметика: multiSeriesSVG кладёт точку по её ИНДЕКСУ внутри ряда,
// а группы (диски/интерфейсы) возвращают разную длину — без общей сетки они разъехались бы по времени.
func namedSeriesFromGroups(groups []metric.GroupedSeries, scale float64, from, to time.Time, step time.Duration) ([]NamedSeries, []templates.LegendItem) {
	series := make([]NamedSeries, len(groups))
	legend := make([]templates.LegendItem, len(groups))
	for i, g := range groups {
		pts := g.Points
		if scale != 1 {
			pts = make([]metric.Point, len(g.Points))
			for j, p := range g.Points {
				pts[j] = metric.Point{T: p.T, V: p.V * scale}
			}
		}
		series[i] = NamedSeries{Label: g.Key, Points: hostGapFill(pts, from, to, step)}
		legend[i] = templates.LegendItem{Label: g.Key, Class: "legend-m" + strconv.Itoa(i+1)}
	}
	return series, legend
}
