package web

import (
	"context"
	"math"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// hostChartWidth/hostChartHeight — размер SVG-графиков карточки хоста.
// Аспект 960×210 (≈4.6:1) шире квадратного графика метрики: графики хоста
// стоят во всю ширину друг под другом (см. .host-charts), и широкий формат
// даёт крупные читаемые оси без чрезмерной высоты стека.
const (
	hostChartWidth  = 960
	hostChartHeight = 210
)

// hostGroupLabelKeys — ЗАКРЫТЫЕ enum-значения атрибутов OTel, по которым
// разбиты группы графиков хоста, и их i18n-ключи: direction у system.disk.io
// (read/write) и system.network.io (receive/transmit), status у
// system.processes.count. Без перевода легенда печатала их сырыми — тот же
// класс находки, что уже правили в разделах метрик и мониторов (UX-аудит A1,
// P2-5).
//
// Карта, а не конкатенация "hosts.legend."+key: множество значений здесь
// открыто снизу — набор status зависит от версии hostmetricsreceiver и ядра,
// и в карте лежат одиннадцать статусов, а не весь возможный набор (те же
// detached/locked доедут до легенды сырыми, и это нормально). Незнакомое
// значение обязано выглядеть КАК ЕСТЬ, а не сырым i18n-ключом
// «hosts.legend.<новое>». По той же причине mountpoint у графика занятости
// диска здесь отсутствует вовсе: это не enum, а произвольный путь.
//
// Резолв всех ключей карты в обеих локалях закрепляет
// TestHostGroupLabelKeysResolve (hostcharts_test.go) — тем же приёмом, каким
// TestDynamicKeysResolve страхует конкатенации.
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

// localizeGroupLabels переводит подписи групп по hostGroupLabelKeys — на
// месте, в уже собранных параллельных срезах series/legend (их строит
// namedSeriesFromGroups из одного и того же g.Key, поэтому индексы совпадают
// по построению; проверка длин страхует от будущего расхождения). Значение,
// которого нет в карте, остаётся сырым — см. докблок карты.
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

// hostGapFill дозаполняет окно [from,to) пустыми (NaN) корзинами по сетке
// step — как в metricDetail: линия рвётся на пропусках, ось X идёт по всему
// выбранному интервалу, а не только по диапазону с данными.
func hostGapFill(points []metric.Point, from, to time.Time, step time.Duration) []metric.Point {
	return fillSeries(points, from, to, step,
		func(p metric.Point) time.Time { return p.T },
		func(t time.Time) metric.Point { return metric.Point{T: t, V: math.NaN()} })
}

// hostDetailCharts собирает семь графиков карточки хоста в порядке §5.3
// дизайна: CPU busy% → RAM% → диск-занятость → диск-IO → сеть → load →
// процессы. Ошибка любого из них — общая 500 (как остальные запросы
// hostDetail): частичная карточка хуже честной ошибки.
func (h *Handler) hostDetailCharts(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings) ([]templates.HostChartVM, error) {
	cpu, err := h.hostCPUChart(ctx, projectID, name, from, to, step)
	if err != nil {
		return nil, err
	}
	mem, err := h.hostMemChart(ctx, projectID, name, from, to, step, settings)
	if err != nil {
		return nil, err
	}
	diskUsage, err := h.hostDiskUsageChart(ctx, projectID, name, from, to, step, settings)
	if err != nil {
		return nil, err
	}
	diskIO, err := h.hostRateChart(ctx, projectID, hostmetric.DiskIO, name, from, to, step, "disk_io")
	if err != nil {
		return nil, err
	}
	net, err := h.hostRateChart(ctx, projectID, hostmetric.NetworkIO, name, from, to, step, "net")
	if err != nil {
		return nil, err
	}
	load, err := h.hostLoadChart(ctx, projectID, name, from, to, step, settings)
	if err != nil {
		return nil, err
	}
	proc, err := h.hostProcChart(ctx, projectID, name, from, to, step)
	if err != nil {
		return nil, err
	}
	return []templates.HostChartVM{cpu, mem, diskUsage, diskIO, net, load, proc}, nil
}

// hostCPUChart — §5.3 №1: scalar Series matcher state=idle, agg=avg, busy =
// 1−v. Пусто (совсем нет точек по запросу, до дозаполнения окна) — карточка
// пустого состояния со скрейпером cpu вместо графика.
func (h *Handler) hostCPUChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration) (templates.HostChartVM, error) {
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
		Chart:  multiSeriesSVG(ctx, series, "%", nil, hostChartWidth, hostChartHeight),
		Legend: []templates.LegendItem{{Label: i18n.T(ctx, "hosts.chart.cpu"), Class: "legend-m1"}},
	}, nil
}

// hostMemChart — §5.3 №2: scalar Series matcher state=used, agg=avg, ×100 —
// значения хранятся долями [0,1] (как везде в host-пороговой подсистеме),
// на графике — проценты. Пороговая линия — settings.MemoryThreshold, только
// если порог включён (выключенный порог не оценивается — «нет данных ≠
// инцидент» не относится к линии, но рисовать линию выключенного порога
// вводило бы в заблуждение: он не действует).
func (h *Handler) hostMemChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings) (templates.HostChartVM, error) {
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
		Chart:  multiSeriesSVG(ctx, series, "%", thresholds, hostChartWidth, hostChartHeight),
		Legend: []templates.LegendItem{{Label: i18n.T(ctx, "hosts.chart.mem"), Class: "legend-m1"}},
	}, nil
}

// hostDiskUsageChart — §5.3 №3: SeriesGrouped по mountpoint, top-8, ×100 —
// та же семантика долей, что у mem. Truncated (>8 mountpoint'ов) едет в VM
// для подписи «показаны топ-8» в шаблоне.
func (h *Handler) hostDiskUsageChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings) (templates.HostChartVM, error) {
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
		Chart:     multiSeriesSVG(ctx, series, "%", thresholds, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

// hostRateChart — §5.3 №4/№5 (диск-IO и сеть, оба SeriesGroupedRate с
// groupKey=direction, deviceKey=device — сумма скоростей по устройствам).
// key — "disk_io"/"net", источник i18n-подписи заголовка/пустого состояния
// (hosts.chart.<key>/hosts.scraper_hint.<key>) и data-chart в шаблоне.
func (h *Handler) hostRateChart(ctx context.Context, projectID int64, metricName, hostName string, from, to time.Time, step time.Duration, key string) (templates.HostChartVM, error) {
	result, err := h.Metrics.SeriesGroupedRate(ctx, projectID, metricName, hostName, hostmetric.AttrDirection, hostmetric.AttrDevice, from, to, step)
	if err != nil {
		return templates.HostChartVM{}, err
	}
	if len(result.Groups) == 0 {
		return templates.HostChartVM{Key: key, Empty: true}, nil
	}
	series, legend := namedSeriesFromGroups(result.Groups, 1, from, to, step)
	localizeGroupLabels(ctx, series, legend)
	// Юнит оси — байты в секунду: и disk.io, и network.io считаются
	// SeriesGroupedRate, то есть это скорость. Без него ось печатала голое
	// «1.2M», и читатель не мог отличить байты от пакетов (UX-аудит A1,
	// P2-4/P2-8).
	return templates.HostChartVM{
		Key:       key,
		Chart:     multiSeriesSVG(ctx, series, i18n.T(ctx, "hosts.chart.unit.bytes_per_second"), nil, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

// hostLoadChart — §5.3 №6: три scalar Series (load_average.1m/5m/15m) плюс
// пунктирный порог «ядра × LoadThreshold», когда порог включён и текущее
// число логических ядер известно (LatestByHost — короткое окно, как в
// списке хостов: делитель должен быть СЕЙЧАС, а не средним за произвольный
// выбранный период графика). Пусто — только если ВСЕ три ряда пусты: одна
// отсутствующая линия (например, коллектор не отдаёт 15m) рисуется разрывом
// через NaN у остальных, а не гасит весь график.
func (h *Handler) hostLoadChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration, settings host.Settings) (templates.HostChartVM, error) {
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
		Chart:  multiSeriesSVG(ctx, series, "", thresholds, hostChartWidth, hostChartHeight),
		Legend: legend,
	}, nil
}

// hostLoadSeries собирает три ряда load average на ОБЩЕЙ сетке [from,to)/step
// — то же дозаполнение, что у namedSeriesFromGroups, и по той же причине
// (multiSeriesSVG кладёт точку по её индексу внутри ряда).
//
// Отдельная функция, а не три строки внутри hostLoadChart: это единственное
// место, где виден частичный отказ рядов (T15 — коллектор отдаёт 1m/5m, но не
// 15m), и проверять его хочется без ClickHouse. Ряд, у которого точек нет
// вовсе, становится сплошным NaN нужной длины — то есть просто не рисуется,
// не сдвигая и не сжимая соседей.
func hostLoadSeries(raw [][]metric.Point, labels []string, from, to time.Time, step time.Duration) ([]NamedSeries, []templates.LegendItem) {
	series := make([]NamedSeries, len(labels))
	legend := make([]templates.LegendItem, len(labels))
	for i := range labels {
		series[i] = NamedSeries{Label: labels[i], Points: hostGapFill(raw[i], from, to, step)}
		legend[i] = templates.LegendItem{Label: labels[i], Class: "legend-m" + strconv.Itoa(i+1)}
	}
	return series, legend
}

// hostCores читает текущее число логических ядер хоста (LatestByHost, окно
// hostsListWindow от РЕАЛЬНОГО «сейчас» — не от границы выбранного графиком
// периода: делитель порога load должен отражать ядра сервера сейчас, а не
// какой-то момент в прошлом произвольного диапазона).
func (h *Handler) hostCores(ctx context.Context, projectID int64, name string) (float64, bool, error) {
	now := time.Now()
	byHost, err := h.Metrics.LatestByHost(ctx, projectID, hostmetric.CPULogicalCount, nil, "", "", now.Add(-hostsListWindow), now)
	if err != nil {
		return 0, false, err
	}
	v, ok := byHost[name]
	return v, ok, nil
}

// hostProcChart — §5.3 №7: SeriesGrouped по status, agg=avg, без масштаба —
// количество процессов, не доля.
func (h *Handler) hostProcChart(ctx context.Context, projectID int64, name string, from, to time.Time, step time.Duration) (templates.HostChartVM, error) {
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
		Chart:     multiSeriesSVG(ctx, series, "", nil, hostChartWidth, hostChartHeight),
		Legend:    legend,
		Truncated: result.Truncated,
	}, nil
}

// namedSeriesFromGroups конвертирует GroupedSeriesResult.Groups (порядок уже
// по убыванию среднего — topNSeriesGroups) в NamedSeries/LegendItem для
// multiSeriesMarkup, домножая значения на scale (100 для долей→проценты, 1
// для сырых величин — байт/с, штук) и дозаполняя КАЖДУЮ группу до общей сетки
// [from,to)/step тем же hostGapFill, что скалярные графики.
//
// Дозаполнение обязательно, а не косметика (ревью I4): multiSeriesSVG кладёт
// точку номер j на ось по её ИНДЕКСУ внутри своего ряда (svg.go, xForIndex(j,
// len(s.Points))), то есть растягивает каждый ряд на всю ширину графика по его
// собственной длине. SeriesGrouped/SeriesGroupedRate возвращают только непустые
// корзины группы, и длины у групп разные: диск, подключённый час назад, или
// интерфейс, скрейпящийся реже соседа, рисовался бы сжатым в чужой временной
// масштаб, а полоса наведения — общая на индекс сетки — показывала бы его
// значение под временем соседней серии. Общая сетка выравнивает индексы, а
// отсутствующие корзины становятся NaN, то есть честным разрывом линии.
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
