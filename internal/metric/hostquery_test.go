package metric

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// seedSumCumulativeHost — как seedSumCumulative (query_extra_test.go), но с
// host и attributes: нужен для SeriesGroupedRate, где device/direction — это
// атрибуты, а host — обязательный аргумент метода.
func seedSumCumulativeHost(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, host string, ts time.Time, val float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'sum', '1', 'api', 'prod', ?, ?, ?, ?, 0, [], [], 1, 'cumulative')`,
		projectID, name, host, attrs, ts, val); err != nil {
		t.Fatalf("seed sum cumulative host: %v", err)
	}
}

// TestSeriesGroupedRateSumsDevices: сердце B2 ревью — rate группы должен быть
// СУММОЙ СКОРОСТЕЙ устройств мелкой размерности (groupKey, deviceKey), а не
// rate от max(value) по группе без разбивки на device (тот вариант дал бы
// 100/60 — скорость только eth0, скорость lo потерялась бы в max()).
func TestSeriesGroupedRateSumsDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 84

	base := now.Add(-5 * time.Minute)
	for i := 0; i <= 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "web-1", ts, float64(100*i),
			map[string]string{"direction": "receive", "device": "eth0"})
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "web-1", ts, float64(10*i),
			map[string]string{"direction": "receive", "device": "lo"})
	}

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	res, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "web-1",
		"direction", "device", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGroupedRate: %v", err)
	}
	if len(res.Groups) != 1 || res.Groups[0].Key != "receive" {
		t.Fatalf("groups = %+v, want одна группа receive", res.Groups)
	}
	if len(res.Groups[0].Points) != 5 {
		t.Fatalf("points = %+v, want 5 бакетов", res.Groups[0].Points)
	}
	want := (100.0 + 10.0) / 60.0
	for _, p := range res.Groups[0].Points {
		if p.V < want-0.01 || p.V > want+0.01 {
			t.Fatalf("point %v = %v, want ≈ %v (сумма скоростей устройств, не rate от max по устройствам)",
				p.T, p.V, want)
		}
	}
}

// TestSeriesGroupedRateSparseDevice: сценарий спеки §8 «отсутствие ложных
// нулей» — eth0 отдаёт точку каждую минуту (=шаг), lo — раз в 3 минуты
// (скрейп реже шага, разъехавшиеся наборы бакетов). Скорость lo между её
// точками обязана «размазаться» на пропущенные бакеты, иначе сумма receive
// проседает в бакетах, где именно у lo нет точки.
func TestSeriesGroupedRateSparseDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 85

	base := now.Add(-9 * time.Minute)
	// eth0: точка каждую минуту, кумулятив +60 за минуту → rate = 1.0/s.
	for i := 0; i <= 9; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "web-1", ts, float64(60*i),
			map[string]string{"direction": "receive", "device": "eth0"})
	}
	// lo: точка раз в 3 минуты, кумулятив +180 за 3 минуты → тот же rate 1.0/s,
	// но БЕЗ размазывания эти минуты (i%3!=0) вообще не получили бы вклада lo.
	for i := 0; i <= 9; i += 3 {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "web-1", ts, float64(60*i),
			map[string]string{"direction": "receive", "device": "lo"})
	}

	from, to := now.Add(-15*time.Minute), now.Add(time.Minute)
	res, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "web-1",
		"direction", "device", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGroupedRate: %v", err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("groups = %+v, want одна группа receive", res.Groups)
	}
	pts := res.Groups[0].Points
	if len(pts) != 9 {
		t.Fatalf("points = %+v, want 9 бакетов (по числу дельт eth0)", pts)
	}
	const eth0Rate = 1.0
	const loRate = 1.0
	want := eth0Rate + loRate
	for _, p := range pts {
		if p.V < eth0Rate-0.01 {
			t.Fatalf("bucket %v = %v, ниже скорости одного eth0 (%v) — ложный провал", p.T, p.V, eth0Rate)
		}
		if p.V < want-0.01 || p.V > want+0.01 {
			t.Fatalf("bucket %v = %v, want ≈ %v (eth0 + размазанная средняя скорость lo, а не eth0+0)",
				p.T, p.V, want)
		}
	}
}

// TestSeriesGroupedRateZeroStepNoPanic: step=0 раньше утекал некэмпленным в
// Go-арифметику размазывания (n := gap/step) и паниковал integer divide by
// zero при ≥2 точках устройства. step=0 обязан клэмпнуться к 1s точно так же,
// как клэмпится stepSec для SQL — результат должен совпасть с явным step=1s.
func TestSeriesGroupedRateZeroStepNoPanic(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 88

	base := now.Add(-3 * time.Minute)
	for i := 0; i <= 3; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "web-1", ts, float64(60*i),
			map[string]string{"direction": "receive", "device": "eth0"})
	}

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	resZero, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "web-1",
		"direction", "device", from, to, 0)
	if err != nil {
		t.Fatalf("SeriesGroupedRate step=0: %v", err)
	}
	resOneSec, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "web-1",
		"direction", "device", from, to, time.Second)
	if err != nil {
		t.Fatalf("SeriesGroupedRate step=1s: %v", err)
	}
	if len(resZero.Groups) != 1 || len(resOneSec.Groups) != 1 {
		t.Fatalf("groups: step=0 → %+v, step=1s → %+v", resZero.Groups, resOneSec.Groups)
	}
	ptsZero, ptsOneSec := resZero.Groups[0].Points, resOneSec.Groups[0].Points
	if len(ptsZero) != len(ptsOneSec) {
		t.Fatalf("points count: step=0 → %d, step=1s → %d", len(ptsZero), len(ptsOneSec))
	}
	for i := range ptsZero {
		if !ptsZero[i].T.Equal(ptsOneSec[i].T) || ptsZero[i].V != ptsOneSec[i].V {
			t.Fatalf("point %d: step=0 → %+v, step=1s → %+v", i, ptsZero[i], ptsOneSec[i])
		}
	}
}

// TestSeriesGroupedTopNTruncates: 10 mountpoint'ов на одном хосте → не больше
// MaxSeriesGroups групп, Truncated=true, отброшены группы с наименьшим средним.
func TestSeriesGroupedTopNTruncates(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 86

	for i := 0; i < 10; i++ {
		mp := fmt.Sprintf("/mnt%d", i)
		seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
			now.Add(-1*time.Minute), float64(i)/10.0, map[string]string{"mountpoint": mp})
	}

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	res, err := q.SeriesGrouped(ctx, pid, "system.filesystem.utilization", "web-1",
		"mountpoint", "avg", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGrouped: %v", err)
	}
	if len(res.Groups) != MaxSeriesGroups {
		t.Fatalf("groups = %d, want %d", len(res.Groups), MaxSeriesGroups)
	}
	if !res.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	// /mnt0 (avg 0.0) и /mnt1 (avg 0.1) — два наименьших средних, обязаны отсеяться.
	for _, g := range res.Groups {
		if g.Key == "/mnt0" || g.Key == "/mnt1" {
			t.Fatalf("group %s не должна пройти top-%d (наименьшее среднее)", g.Key, MaxSeriesGroups)
		}
	}
}

// TestSeriesGroupedScalar: два mountpoint'а → две группы, значение каждой
// точки — avg по бакету, группы отсортированы по убыванию среднего (для
// стабильной легенды графика).
func TestSeriesGroupedScalar(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 87

	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-2*time.Minute), 0.2, map[string]string{"mountpoint": "/"})
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.4, map[string]string{"mountpoint": "/"})
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.9, map[string]string{"mountpoint": "/var"})

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	res, err := q.SeriesGrouped(ctx, pid, "system.filesystem.utilization", "web-1",
		"mountpoint", "avg", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGrouped: %v", err)
	}
	if len(res.Groups) != 2 {
		t.Fatalf("groups = %+v, want 2", res.Groups)
	}
	if res.Truncated {
		t.Fatalf("Truncated = true, want false")
	}
	if res.Groups[0].Key != "/var" {
		t.Fatalf("groups[0] = %s, want /var первой (наибольшее среднее 0.9)", res.Groups[0].Key)
	}
	var root GroupedSeries
	for _, g := range res.Groups {
		if g.Key == "/" {
			root = g
		}
	}
	if len(root.Points) != 2 || root.Points[0].V != 0.2 || root.Points[1].V != 0.4 {
		t.Fatalf("root points = %+v, want [0.2, 0.4] по бакетам", root.Points)
	}
}

// TestSeriesGroupedEmptyHostAllHosts: host=="" обязан означать «все хосты»
// (симметрия с Series/scalarSeries), а не «строки с буквально пустым host» —
// рецепты сервисов B6 живут без resourcedetection, их метрики приходят с
// пустым host, и жёсткое равенство host оставляло график рецепта пустым. Контроль:
// непустой host по-прежнему фильтрует только свои строки.
func TestSeriesGroupedEmptyHostAllHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 89

	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "srv-1",
		now.Add(-2*time.Minute), 0.5, map[string]string{"mountpoint": "/"})
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "srv-2",
		now.Add(-2*time.Minute), 0.7, map[string]string{"mountpoint": "/data"})

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	// host="" → точки обоих хостов видны (две группы по mountpoint).
	res, err := q.SeriesGrouped(ctx, pid, "system.filesystem.utilization", "",
		"mountpoint", "avg", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGrouped host=empty: %v", err)
	}
	if len(res.Groups) != 2 {
		t.Fatalf("groups host=empty = %+v, want 2 (points of all hosts)", res.Groups)
	}

	// Контроль: host="srv-1" фильтрует только его.
	res1, err := q.SeriesGrouped(ctx, pid, "system.filesystem.utilization", "srv-1",
		"mountpoint", "avg", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGrouped host=srv-1: %v", err)
	}
	if len(res1.Groups) != 1 || res1.Groups[0].Key != "/" {
		t.Fatalf("groups host=srv-1 = %+v, want only group / of srv-1", res1.Groups)
	}
}

// TestSeriesGroupedRateEmptyHostAllHosts: тот же пустой-байпас host для
// rate-версии — счётчики без host-атрибуции должны быть видны при host=="".
func TestSeriesGroupedRateEmptyHostAllHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 90

	base := now.Add(-3 * time.Minute)
	for i := 0; i <= 3; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, "system.network.io", "srv-1", ts, float64(60*i),
			map[string]string{"direction": "receive", "device": "eth0"})
	}

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	// host="" → точки srv-1 видны.
	res, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "",
		"direction", "device", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGroupedRate host=empty: %v", err)
	}
	if len(res.Groups) != 1 || res.Groups[0].Key != "receive" {
		t.Fatalf("groups host=empty = %+v, want one group receive", res.Groups)
	}
	if len(res.Groups[0].Points) == 0 {
		t.Fatalf("points host=empty are empty, want rate points of srv-1")
	}

	// Контроль: чужой host по-прежнему отфильтровывает всё.
	resOther, err := q.SeriesGroupedRate(ctx, pid, "system.network.io", "srv-2",
		"direction", "device", from, to, time.Minute)
	if err != nil {
		t.Fatalf("SeriesGroupedRate host=srv-2: %v", err)
	}
	if len(resOther.Groups) != 0 {
		t.Fatalf("groups host=srv-2 = %+v, want empty (foreign host)", resOther.Groups)
	}
}

// TestLatestByHostWorstMountpoint: у web-1 два лейбла mountpoint внутри метрики
// (/, /var) и у /var есть СТАРАЯ точка-приманка со значением 0.10 до свежих
// 0.30 и 0.95. Одноуровневый argMax по host взял бы значение ОДНОГО случайного
// mountpoint (или, что хуже, старую приманку) — правильный ответ 0.95 получается
// только двухуровневым агрегатом: argMax(value, ts) по (host, mountpoint), затем
// max по host.
func TestLatestByHostWorstMountpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 81

	// Старая точка-приманка на /var: раньше свежих, но с "заманчивым" низким значением.
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-30*time.Minute), 0.10, map[string]string{"mountpoint": "/var"})
	// Свежие точки.
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.30, map[string]string{"mountpoint": "/"})
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.95, map[string]string{"mountpoint": "/var"})

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	got, err := q.LatestByHost(ctx, pid, "system.filesystem.utilization", nil,
		"mountpoint", "max", from, to)
	if err != nil {
		t.Fatalf("LatestByHost: %v", err)
	}
	if v, ok := got["web-1"]; !ok || v != 0.95 {
		t.Fatalf("got[web-1] = %v (ok=%v), want 0.95", v, ok)
	}
}

// TestLatestByHostAvgOverCPUs: усреднение последних значений по ядрам (cpu=0,1)
// state=idle для одного хоста.
func TestLatestByHostAvgOverCPUs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 82

	seedGaugeHost(t, conn, pid, "system.cpu.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.8, map[string]string{"state": "idle", "cpu": "0"})
	seedGaugeHost(t, conn, pid, "system.cpu.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.4, map[string]string{"state": "idle", "cpu": "1"})
	// Другой state — не должен участвовать (отсекается матчером).
	seedGaugeHost(t, conn, pid, "system.cpu.utilization", "prod", "web-1",
		now.Add(-1*time.Minute), 0.99, map[string]string{"state": "used", "cpu": "0"})

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	got, err := q.LatestByHost(ctx, pid, "system.cpu.utilization",
		[]LabelMatcher{{Key: "state", Value: "idle"}}, "cpu", "avg", from, to)
	if err != nil {
		t.Fatalf("LatestByHost avg: %v", err)
	}
	if v, ok := got["web-1"]; !ok || v < 0.6-0.001 || v > 0.6+0.001 {
		t.Fatalf("got[web-1] = %v (ok=%v), want 0.6", v, ok)
	}
}

// TestHostsListsActivity: два хоста, у каждого своя max(ts); Hosts возвращает
// обе записи, отсортированные по имени хоста.
func TestHostsListsActivity(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 83

	web1Last := now.Add(-1 * time.Minute)
	web2Last := now.Add(-5 * time.Minute)
	seedGaugeHost(t, conn, pid, "m", "prod", "web-1", now.Add(-9*time.Minute), 1, nil)
	seedGaugeHost(t, conn, pid, "m", "prod", "web-1", web1Last, 2, nil)
	seedGaugeHost(t, conn, pid, "m", "prod", "web-2", web2Last, 3, nil)
	// Точка без host не должна попасть в перечень (host != '').
	seedGauge(t, conn, pid, "m", "prod", now.Add(-1*time.Minute), 5, nil)

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	got, err := q.Hosts(ctx, pid, from, to)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Hosts = %+v, want 2 записи", got)
	}
	if got[0].Host != "web-1" || !got[0].LastTS.Equal(web1Last) {
		t.Fatalf("got[0] = %+v, want web-1 @ %v", got[0], web1Last)
	}
	if got[1].Host != "web-2" || !got[1].LastTS.Equal(web2Last) {
		t.Fatalf("got[1] = %+v, want web-2 @ %v", got[1], web2Last)
	}
}
